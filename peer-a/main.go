package main

import (
	"fmt"
	"log"
	"net"
	"time"
)

const (
	peerBAddress = "172.29.1.2:8080"
	localPort    = ":8080"
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

	// Start a goroutine to listen for incoming messages
	go func() {
		buffer := make([]byte, 1024)
		for {
			n, addr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				log.Printf("Error reading from UDP: %v", err)
				continue
			}
			fmt.Printf("Received from %s: %s\n", addr, string(buffer[:n]))
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

	// Send the "Goodjob" message
	fmt.Println("Sending 'Goodjob' message...")
	_, err = conn.WriteToUDP([]byte("Goodjob"), remoteAddr)
	if err != nil {
		log.Fatalf("Failed to send 'Goodjob' message: %v", err)
	}

	// Keep the application running to receive potential responses
	fmt.Println("Message sent. Peer A will keep running to listen for responses.")
	select {} // Block forever
}
