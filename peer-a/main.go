package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// peerBAddress = "172.29.1.2:8080"
	peerBAddress = "192.168.1.18:8080"
	localPort    = ":8080"
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

	// Channels and state for handshake flow
	replyAddrCh := make(chan *net.UDPAddr, 1) // first non-punch reply's sender
	var sentInitial int32                     // 0: not yet sent, 1: sent

	// Start a goroutine to listen for incoming messages
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, addr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				log.Printf("Error reading from UDP: %v", err)
				continue
			}
			message := string(buffer[:n])
			fmt.Printf("Received from %s: %s\n", addr, message)
			// Ignore punch packets
			if message == "punch" {
				continue
			}
			// If a SNY message arrives before we've sent our initial SNY, it's a protocol violation
			if strings.HasPrefix(message, "SNY:") && atomic.LoadInt32(&sentInitial) == 0 {
				log.Fatalf("Protocol violation: received step 2 before step 1. Aborting.")
				return
			}
			// Capture the first non-punch reply's sender to send 'finish' back (expects SNY:...)
			if strings.HasPrefix(message, "SNY:") {
				select {
				case replyAddrCh <- addr:
				default:
				}
				return
			}
		}
	}()

	// --- UDP Hole Punching ---
	// Send a few initial packets to "punch a hole" in the NAT.
	fmt.Println("Sending punching packets...")
	for i := 0; i < 3; i++ {
		_, err := conn.WriteToUDP([]byte("punch"), remoteAddr)
		if err != nil {
			log.Printf("Error sending punch packet: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Send an initial arbitrary message with required prefix
	payload := "~~~" // 任意の文字列
	msg := "SNY:" + payload
	fmt.Printf("Sending %q message...\n", msg)
	// Mark that step 1 will be sent to avoid race where reply arrives extremely fast
	atomic.StoreInt32(&sentInitial, 1)
	_, err = conn.WriteToUDP([]byte(msg), remoteAddr)
	if err != nil {
		log.Fatalf("Failed to send %q message: %v", msg, err)
	}

	// Wait for any non-punch reply, then send 'finish' and end
	fmt.Println("Message sent. Waiting for a reply to send 'finish'...")
	select {
	case addr := <-replyAddrCh:
		fmt.Println("Sending 'finish' in response...")
		if _, err := conn.WriteToUDP([]byte("finish"), addr); err != nil {
			log.Fatalf("Error sending 'finish': %v", err)
		}
		fmt.Println("Successfully finished.")
		return
	case <-time.After(10 * time.Second):
		log.Fatalf("Timeout: Did not receive a reply within 10 seconds.")
	}
}
