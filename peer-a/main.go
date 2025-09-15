package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
)

const (
	// peerBAddress = "172.29.1.2:8080"
	peerBAddress = "192.168.1.18:8080"
	localPort    = ":8080"
)

const (
	chunkSize        = 16384            // 16KB
	maxBufferedBytes = 16 * 1024 * 1024 // 16MB
)

func main() {
	// Resolve local and remote addresses
	localAddr, err := net.ResolveUDPAddr("udp", localPort)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %v", err)
	}

	// Allow overriding remote address via environment variable
	remoteAddrStr := peerBAddress
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

	fmt.Printf("Peer A listening on %s\n", conn.LocalAddr().String())
	fmt.Printf("Will send messages to Peer B at %s\n", remoteAddr.String())

	// Start punching to create/refresh NAT bindings
	punch(conn, remoteAddr)

	// Prepare WebRTC peer connection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Fatalf("Peer A: create pc: %v", err)
	}
	defer pc.Close()

	// Create a datachannel and send a hello or a file when open
	dc, err := pc.CreateDataChannel("p2p", nil)
	if err != nil {
		log.Fatalf("Peer A: create datachannel: %v", err)
	}
	dc.OnOpen(func() {
		sendPath := os.Getenv("SEND_FILE")
		if sendPath == "" {
			fmt.Println("Peer A: DataChannel open -> sending greeting (no SEND_FILE)")
			_ = dc.SendText("hello from A")
			return
		}
		// normalize path and check
		if fi, err := os.Stat(sendPath); err != nil || fi.IsDir() {
			fmt.Printf("Peer A: SEND_FILE invalid: %v\n", err)
			_ = dc.SendText("hello from A (file missing)")
			return
		}

		// Send metadata
		fi, _ := os.Stat(sendPath)
		meta := map[string]interface{}{
			"name": filepath.Base(sendPath),
			"size": fi.Size(),
			"ts":   time.Now().Unix(),
			"ver":  1,
		}
		metaBytes, _ := json.Marshal(meta)
		if err := dc.SendText(string(metaBytes)); err != nil {
			log.Printf("Peer A: send metadata error: %v", err)
			return
		}
		fmt.Printf("Peer A: Sending file %s (%d bytes)\n", fi.Name(), fi.Size())
		go func() {
			if err := sendFile(dc, sendPath, fi.Size()); err != nil {
				log.Printf("Peer A: sendFile error: %v", err)
			}
			// Give time for last packets to flush, then close DC and PC
			time.Sleep(500 * time.Millisecond)
			_ = dc.Close()
			_ = pc.Close()
		}()
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			fmt.Printf("Peer A: got message: %s\n", string(m.Data))
		} else {
			fmt.Printf("Peer A: got %d bytes\n", len(m.Data))
		}
	})

	// Offerer flow
	gatheringComplete := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Fatalf("Peer A: CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		log.Fatalf("Peer A: SetLocalDescription: %v", err)
	}
	<-gatheringComplete
	encOffer, err := encode(*pc.LocalDescription())
	if err != nil {
		log.Fatalf("Peer A: encode offer: %v", err)
	}

	// Send session ID first via UDP
	sid := uuid.NewString()
	mustWriteUDP(conn, remoteAddr, fmt.Sprintf("ID:%s", sid))
	fmt.Printf("Peer A: Sent session ID %s\n", sid)

	// Send the OFFER via UDP
	mustWriteUDP(conn, remoteAddr, "OFFER:"+encOffer)
	fmt.Println("Peer A: Sent OFFER via UDP")

	// Wait for ANSWER via UDP and apply
	answerStr, from, err := waitForPrefix(conn, "ANSWER:", 30*time.Second)
	if err != nil {
		log.Fatalf("Peer A: waiting ANSWER: %v", err)
	}
	fmt.Printf("Peer A: Got ANSWER from %s (%d bytes)\n", from.String(), len(answerStr))

	var answer webrtc.SessionDescription
	if err := decode(answerStr, &answer); err != nil {
		log.Fatalf("Peer A: decode answer: %v", err)
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		log.Fatalf("Peer A: SetRemoteDescription: %v", err)
	}

	// Keep the process alive until connection finishes
	done := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer A: Connection state -> %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateDisconnected || s == webrtc.PeerConnectionStateClosed {
			close(done)
		}
	})

	<-done
	fmt.Println("Peer A: exiting")
}

func punch(conn *net.UDPConn, remote *net.UDPAddr) {
	fmt.Println("Peer A: Sending punching packets...")
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
		fmt.Printf("Peer A: Ignoring UDP '%s' from %s\n", preview(msg), addr)
	}
}

func mustWriteUDP(conn *net.UDPConn, addr *net.UDPAddr, s string) {
	// Small helper with simple retry
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

// sendFile streams the file to the DataChannel with simple backpressure
func sendFile(dc *webrtc.DataChannel, filePath string, total int64) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 1*1024*1024)
	buf := make([]byte, chunkSize)
	var sent int64
	start := time.Now()

	// Backpressure support
	lowThreshold := uint64(maxBufferedBytes / 2)
	dc.SetBufferedAmountLowThreshold(lowThreshold)
	wake := make(chan struct{}, 1)
	dc.OnBufferedAmountLow(func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				printSenderProgress(dc.Label(), sent, total, start)
			case <-done:
				printSenderProgress(dc.Label(), sent, total, start)
				fmt.Println()
				return
			}
		}
	}()

	for {
		n, rerr := reader.Read(buf)
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			close(done)
			return rerr
		}

		// Wait for buffer to drain
		for dc.BufferedAmount() > maxBufferedBytes {
			select {
			case <-wake:
			case <-time.After(5 * time.Millisecond):
			}
		}
		if err := dc.Send(buf[:n]); err != nil {
			close(done)
			return err
		}
		sent += int64(n)
	}
	close(done)
	fmt.Println("Peer A: File sent successfully.")
	return nil
}

func printSenderProgress(label string, sent, total int64, start time.Time) {
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	speed := float64(sent) / elapsed
	pct := 0.0
	if total > 0 {
		pct = 100 * float64(sent) / float64(total)
	}
	etaStr := "ETA --"
	if total > 0 && speed > 0 {
		rem := float64(total - sent)
		eta := rem / speed
		etaStr = fmt.Sprintf("ETA %ds", int(eta))
	}
	fmt.Printf("\rSender: %s | %6.2f%% | %d/%d bytes | %.2f MB/s | %s", label, pct, sent, total, speed/1024.0/1024.0, etaStr)
}
