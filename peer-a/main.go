package main

import (
	"fmt"
	"log"
	"net"
	"time"
)

const (
	peerBAddress = "172.29.1.2:8080"
	// peerBAddress = "192.168.1.18:8080"
	localPort = ":8080"
)

func main() {
	// Resolve local and remote addresses
	localAddr, err := net.ResolveUDPAddr("udp", localPort)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %v", err)
	}

	remoteAddr, err := net.ResolveUDPAddr("udp", peerBAddress)
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

	// Channel to signal when the "Finish" message is received
	done := make(chan struct{})

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
			if message == "Finish" {
				fmt.Println("\n--- Finish received! ---")
				select {
				case done <- struct{}{}:
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

	// Send the "Help me" message
	// Manage outgoing strings in variables
	helpMsg := "~~~"

	fmt.Printf("Sending %q message...\n", helpMsg)
	_, err = conn.WriteToUDP([]byte(helpMsg), remoteAddr)
	if err != nil {
		log.Fatalf("Failed to send %q message: %v", helpMsg, err)
	}

	// Wait for the "Finish" message or timeout
	fmt.Println("Message sent. Waiting for completion (Finish)...")
	select {
	case <-done:
		fmt.Println("Successfully finished.")
		return
	case <-time.After(10 * time.Second):
		log.Fatalf("Timeout: Did not receive 'Finish' message after 10 seconds.")
	}
}
