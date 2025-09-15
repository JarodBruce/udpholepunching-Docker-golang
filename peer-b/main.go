package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
)

const (
	// peerAAddress = "172.29.1.1:8080"
	peerAAddress = "192.168.1.18:8080"
	defaultLocal = ":8080"
)

func main() {
	// Resolve local and remote addresses
	bindAddr := defaultLocal
	if v := os.Getenv("LOCAL_ADDR"); v != "" {
		bindAddr = v
	} else if v := os.Getenv("LOCAL_PORT"); v != "" {
		bindAddr = ":" + v
	}
	// Optional: bind only to localhost to avoid Windows firewall prompts
	if lb := os.Getenv("BIND_LOCALHOST_ONLY"); lb == "1" || strings.EqualFold(lb, "true") {
		if strings.HasPrefix(bindAddr, ":") {
			bindAddr = "127.0.0.1" + bindAddr
		}
	} else {
		// Default to IPv4 for Docker networks unless explicitly binding to IPv6
		if bindAddr == ":8080" {
			bindAddr = "0.0.0.0:8080"
		}
	}
	localAddr, err := net.ResolveUDPAddr("udp", bindAddr)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %v", err)
	}

	// Allow overriding remote address via environment variable or interactive prompt
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

	// Shared receive state across channels
	var recvTotal int64
	var totalExpected int64 = -1
	// filePath removed (not used)
	var file *os.File
	var start time.Time
	var initOnce sync.Once
	var closeOnce sync.Once
	var multiMode bool
	// Determine output directory for received files
	outDirBase := os.Getenv("RECV_DIR")
	if outDirBase == "" {
		outDirBase = "data"
	}
	outDir := outDirBase

	// Handle incoming datachannel with optional file receive
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("Peer B: New DataChannel %s\n", d.Label())
		// per-channel header state for v2 frames
		var haveHeader bool
		var hdrOff uint64
		var hdrLen uint32
		// Add buffering for data received before metadata
		var dataBuffer []webrtc.DataChannelMessage

		// Helper function to process data messages
		processDataMessage := func(msg webrtc.DataChannelMessage, label string) {
			if multiMode {
				// Expect header then payload
				b := msg.Data
				if !haveHeader {
					if len(b) < 12 {
						log.Printf("Peer B: header too small on %s: %d", label, len(b))
						return
					}
					hdrOff = binary.LittleEndian.Uint64(b[0:8])
					hdrLen = binary.LittleEndian.Uint32(b[8:12])
					// if there's inline payload as well
					if len(b) > 12 {
						payload := b[12:]
						if int(hdrLen) != len(payload) {
							log.Printf("Peer B: payload length mismatch: got %d expect %d", len(payload), hdrLen)
						}
						if _, err := file.WriteAt(payload, int64(hdrOff)); err != nil {
							log.Printf("Peer B: writeAt error: %v", err)
							return
						}
						atomic.AddInt64(&recvTotal, int64(len(payload)))
						printRecvProgress("ALL", atomic.LoadInt64(&recvTotal), totalExpected, start)
						haveHeader = false
						return
					}
					haveHeader = true
					return
				}
				// have header: this is payload
				if int(hdrLen) != len(b) {
					// best effort: write what we received
					// In practice sender sends exact sizes
				}
				if _, err := file.WriteAt(b, int64(hdrOff)); err != nil {
					log.Printf("Peer B: writeAt error: %v", err)
					return
				}
				atomic.AddInt64(&recvTotal, int64(len(b)))
				printRecvProgress("ALL", atomic.LoadInt64(&recvTotal), totalExpected, start)
				haveHeader = false
			} else {
				// v1 sequential receive (single channel)
				n, werr := file.Write(msg.Data)
				if werr != nil {
					log.Printf("Peer B: write error: %v", werr)
					return
				}
				atomic.AddInt64(&recvTotal, int64(n))
				printRecvProgress(label, atomic.LoadInt64(&recvTotal), totalExpected, start)
			}

			// completion check
			if totalExpected > 0 && atomic.LoadInt64(&recvTotal) >= totalExpected {
				closeOnce.Do(func() {
					fmt.Println("\nPeer B: File received completely. Closing...")
					if file != nil {
						_ = file.Close()
						file = nil
					}
					_ = d.Close()
					_ = pc.Close()
				})
			}
		}

		d.OnOpen(func() {
			fmt.Println("Peer B: DataChannel open. Waiting for metadata...")
			start = time.Now()
			if fi, err := os.Stat(outDir); err == nil && !fi.IsDir() {
				outDir = outDirBase + "_dir"
			}
			_ = os.MkdirAll(outDir, 0755)
			_ = d.SendText("hello from B")
		})

		d.OnClose(func() {
			fmt.Printf("Peer B: DataChannel %s closed\n", d.Label())
		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString {
				// Expect metadata as JSON
				var meta struct {
					Name     string `json:"name"`
					Size     int64  `json:"size"`
					Ts       int64  `json:"ts"`
					Ver      int    `json:"ver"`
					Channels int    `json:"channels"`
				}
				if err := json.Unmarshal(msg.Data, &meta); err == nil && meta.Name != "" {
					initOnce.Do(func() {
						totalExpected = meta.Size
						multiMode = meta.Ver >= 2 && meta.Channels > 1
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
						if multiMode && totalExpected > 0 {
							// Pre-allocate for random writes
							if err := file.Truncate(totalExpected); err != nil {
								log.Printf("Peer B: truncate error: %v", err)
							}
						}
						fmt.Printf("Peer B: Metadata received. Name=%s Size=%d bytes -> %s (channels=%d, ver=%d)\n", safeName, meta.Size, fp, meta.Channels, meta.Ver)
						// Tell sender we're ready for v2
						_ = d.SendText("READY")
						// Process any buffered data
						for _, bufferedMsg := range dataBuffer {
							processDataMessage(bufferedMsg, d.Label())
						}
						dataBuffer = nil // Clear buffer
					})
					return
				}
				// other text messages ignored
				return
			}

			// Buffer data if file not ready yet
			if file == nil {
				dataBuffer = append(dataBuffer, msg)
				if len(dataBuffer) > 1000 { // Prevent memory overflow
					dataBuffer = dataBuffer[500:] // Keep only recent half
				}
				return
			}

			processDataMessage(msg, d.Label())
		})
	})

	// Receive session ID
	sid, from, err := waitForPrefix(conn, "ID:", 60*time.Second)
	if err != nil {
		log.Fatalf("Peer B: waiting ID: %v", err)
	}
	fmt.Printf("Peer B: Received session ID %s from %s\n", sid, from.String())

	// Receive OFFER via UDP
	offerStr, from2, err := waitForPrefix(conn, "OFFER:", 60*time.Second)
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
	// Send ANSWER via UDP back to the observed sender address of the OFFER
	if _, err := conn.WriteToUDP([]byte("ANSWER:"+encAnswer), from2); err != nil {
		log.Printf("Peer B: failed sending ANSWER to %s: %v", from2.String(), err)
	} else {
		fmt.Printf("Peer B: Sent ANSWER via UDP to %s\n", from2.String())
	}

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
	for i := 0; i < 5; i++ {
		_, _ = conn.WriteToUDP([]byte("punch"), remote)
		time.Sleep(200 * time.Millisecond)
	}
	// Add extra delay to ensure proper hole punching
	time.Sleep(500 * time.Millisecond)
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
