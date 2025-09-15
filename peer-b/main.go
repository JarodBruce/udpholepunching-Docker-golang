package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pion/webrtc/v3"
)

const (
	// peerAAddress = "172.29.1.1:8080"
	peerAAddress = "192.168.1.18:8080"
	localPort    = ":8080"
)

func main() {
	// Resolve local and remote addresses
	localAddr, err := net.ResolveUDPAddr("udp", localPort)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %v", err)
	}

	// Allow overriding remote address via environment variable
	remoteAddrStr := peerAAddress
	if ifEnv, ok := os.LookupEnv("REMOTE_ADDR"); ok && ifEnv != "" {
		remoteAddrStr = ifEnv
	}
	remoteAddr, err := net.ResolveUDPAddr("udp", remoteAddrStr)
	if err != nil {
		log.Fatalf("Failed to resolve remote address: %v", err)
	}

	// Listen on the local UDP port
	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		log.Fatalf("Failed to listen on UDP port: %v", err)
	}
	defer conn.Close()

	fmt.Printf("Peer B listening on %s\n", conn.LocalAddr().String())
	fmt.Printf("Will send messages to Peer A at %s\n", remoteAddr.String())

	// Start punching to create/refresh NAT bindings
	punch(conn, remoteAddr)

	// Prepare WebRTC peer connection (Answerer)
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Fatalf("Peer B: create pc: %v", err)
	}
	defer pc.Close()

	// Handle incoming datachannel with optional file receive
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("Peer B: New DataChannel %s\n", d.Label())
		var file *os.File
		var writer *bufio.Writer
		var total int64 = -1
		var received int64
		var start time.Time
		outDirBase := os.Getenv("RECV_DIR")
		if outDirBase == "" {
			outDirBase = "/data/out"
		}
		outDir := outDirBase

		d.OnOpen(func() {
			fmt.Println("Peer B: DataChannel open. Waiting for metadata...")
			start = time.Now()
			if fi, err := os.Stat(outDir); err == nil && !fi.IsDir() {
				outDir = outDirBase + "_dir"
			}
			_ = os.MkdirAll(outDir, 0755)
			// send a greeting too (harmless)
			_ = d.SendText("hello from B")
		})

		d.OnClose(func() {
			fmt.Println("Peer B: DataChannel closed")
			if writer != nil {
				_ = writer.Flush()
			}
			if file != nil {
				_ = file.Close()
			}
		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString {
				// Expect metadata as JSON
				var meta struct {
					Name string `json:"name"`
					Size int64  `json:"size"`
					Ts   int64  `json:"ts"`
					Ver  int    `json:"ver"`
				}
				if err := json.Unmarshal(msg.Data, &meta); err == nil && meta.Name != "" {
					total = meta.Size
					if fi, err := os.Stat(outDir); err == nil && !fi.IsDir() {
						outDir = outDirBase + "_dir"
					}
					_ = os.MkdirAll(outDir, 0755)
					safeName := filepath.Base(meta.Name)
					if safeName == "." || safeName == "" {
						safeName = fmt.Sprintf("received_%d.bin", time.Now().Unix())
					}
					fp := filepath.Join(outDir, safeName)
					f, openErr := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
					if openErr != nil {
						log.Printf("Peer B: open file error: %v", openErr)
						return
					}
					file = f
					writer = bufio.NewWriterSize(file, 1*1024*1024)
					fmt.Printf("Peer B: Metadata received. Name=%s Size=%d bytes -> %s\n", safeName, meta.Size, fp)
					return
				}
				fmt.Println("Peer B: Unexpected string message, ignoring.")
				return
			}

			if file == nil {
				fmt.Println("Peer B: Error: file not open yet.")
				return
			}
			n, werr := writer.Write(msg.Data)
			if werr != nil {
				log.Printf("Peer B: write error: %v", werr)
				return
			}
			received += int64(n)
			printRecvProgress(d.Label(), received, total, start)

			if total > 0 && received >= total {
				fmt.Println("\nPeer B: File received completely. Closing...")
				if writer != nil {
					_ = writer.Flush()
				}
				if file != nil {
					_ = file.Close()
					file = nil
				}
				// Close DC and PC to terminate
				_ = d.Close()
				_ = pc.Close()
			}
		})
	})

	// Receive session ID
	sid, from, err := waitForPrefix(conn, "ID:", 30*time.Second)
	if err != nil {
		log.Fatalf("Peer B: waiting ID: %v", err)
	}
	fmt.Printf("Peer B: Received session ID %s from %s\n", sid, from.String())

	// Receive OFFER via UDP
	offerStr, from2, err := waitForPrefix(conn, "OFFER:", 30*time.Second)
	if err != nil {
		log.Fatalf("Peer B: waiting OFFER: %v", err)
	}
	fmt.Printf("Peer B: Got OFFER from %s (%d bytes)\n", from2.String(), len(offerStr))

	var offer webrtc.SessionDescription
	if err := decode(offerStr, &offer); err != nil {
		log.Fatalf("Peer B: decode offer: %v", err)
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		log.Fatalf("Peer B: SetRemoteDescription: %v", err)
	}

	gatheringComplete := webrtc.GatheringCompletePromise(pc)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Fatalf("Peer B: CreateAnswer: %v", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Fatalf("Peer B: SetLocalDescription: %v", err)
	}
	<-gatheringComplete

	encAnswer, err := encode(*pc.LocalDescription())
	if err != nil {
		log.Fatalf("Peer B: encode answer: %v", err)
	}
	// Send ANSWER via UDP
	mustWriteUDP(conn, remoteAddr, "ANSWER:"+encAnswer)
	fmt.Println("Peer B: Sent ANSWER via UDP")

	// Keep the process alive until connection finishes
	done := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer B: Connection state -> %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateDisconnected || s == webrtc.PeerConnectionStateClosed {
			close(done)
		}
	})
	<-done
	fmt.Println("Peer B: exiting")
}

func punch(conn *net.UDPConn, remote *net.UDPAddr) {
	fmt.Println("Peer B: Sending punching packets...")
	for i := 0; i < 3; i++ {
		_, _ = conn.WriteToUDP([]byte("punch"), remote)
		time.Sleep(300 * time.Millisecond)
	}
}

func waitForPrefix(conn *net.UDPConn, prefix string, timeout time.Duration) (string, *net.UDPAddr, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", nil, err
		}
		msg := string(buf[:n])
		if msg == "punch" { // ignore keepalive punches
			continue
		}
		if len(msg) >= len(prefix) && strings.HasPrefix(msg, prefix) {
			return msg[len(prefix):], addr, nil
		}
		fmt.Printf("Peer B: Ignoring UDP '%s' from %s\n", preview(msg), addr)
	}
}

func mustWriteUDP(conn *net.UDPConn, addr *net.UDPAddr, s string) {
	for i := 0; i < 3; i++ {
		if _, err := conn.WriteToUDP([]byte(s), addr); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func encode(obj interface{}) (string, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func decode(in string, obj interface{}) error {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, obj)
}

func preview(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

func printRecvProgress(label string, received, total int64, start time.Time) {
	if total <= 0 {
		fmt.Printf("\rReceiver: %s | %d bytes received", label, received)
		return
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	pct := 100 * float64(received) / float64(total)
	speed := float64(received) / elapsed
	rem := float64(total - received)
	eta := rem / speed
	fmt.Printf("\rReceiver: %s | %6.2f%% | %d/%d bytes | %.2f MB/s | ETA %ds",
		label, pct, received, total, speed/1024.0/1024.0, int(eta))
}
