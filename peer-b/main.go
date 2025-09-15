package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"time"
)

const (
	peerAAddress = "172.29.1.1:8080"
	// peerAAddress = "192.168.1.18:8080"
	localPort = ":8080"
)

func main() {
	// Resolve local and remote addresses
	localAddr, err := net.ResolveUDPAddr("udp", localPort)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %v", err)
	}

	remoteAddr, err := net.ResolveUDPAddr("udp", peerAAddress)
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

	// Channel to signal when the "Finish" response has been sent
	done := make(chan bool)

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
			// Ignore punch packets; respond 'Finish' to any other payload
			if message != "punch" {
				fmt.Println("Sending 'Finish' in response...")
				// Reply to the actual sender address to be NAT-friendly
				if _, err := conn.WriteToUDP([]byte("Finish"), addr); err != nil {
					log.Printf("Error sending 'Finish': %v", err)
				} else {
					select {
					case done <- true:
					default:
					}
					return
				}
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

	fmt.Println("Punching packets sent. Waiting for messages...")

	// Wait until we've sent the completion response (Finish) or timeout
	select {
	case <-done:
		fmt.Println("Successfully finished.")
		os.Exit(0)
	case <-time.After(15 * time.Second):
		log.Fatalf("Timeout: Did not receive a non-'punch' message within 15 seconds.")
	}
}
