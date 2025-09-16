package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
)

const (
	peerBAddress = "172.29.1.2:8080"
	// peerBAddress = "10.40.249.136:8080"
	defaultLocal = ":8080"
)

const (
	chunkSize        = 16384            // 16KB
	maxBufferedBytes = 16 * 1024 * 1024 // 16MB
)

func main() {
	bindAddr := defaultLocal
	if v := os.Getenv("LOCAL_ADDR"); v != "" {
		bindAddr = v
	} else if v := os.Getenv("LOCAL_PORT"); v != "" {
		bindAddr = ":" + v
	}
	if lb := os.Getenv("BIND_LOCALHOST_ONLY"); lb == "1" || strings.EqualFold(lb, "true") {
		if strings.HasPrefix(bindAddr, ":") {
			bindAddr = "127.0.0.1" + bindAddr
		}
	} else {
		if bindAddr == ":8080" {
			bindAddr = "0.0.0.0:8080"
		}
	}
	localAddr, err := net.ResolveUDPAddr("udp4", bindAddr)
	if err != nil {
		log.Fatalf("Failed to resolve local address: %v", err)
	}

	remoteAddrStr := peerBAddress
	if ifEnv, ok := os.LookupEnv("REMOTE_ADDR"); ok && ifEnv != "" {
		remoteAddrStr = ifEnv
	}

	remoteAddr, err := net.ResolveUDPAddr("udp4", remoteAddrStr)
	if err != nil {
		log.Fatalf("Failed to resolve remote address: %v", err)
	}

	// Listen on the local UDP port
	conn, err := net.ListenUDP("udp4", localAddr)
	if err != nil {
		log.Fatalf("Failed to listen on UDP port: %v", err)
	}
	defer conn.Close()

	fmt.Printf("Peer A listening on %s\n", conn.LocalAddr().String())
	fmt.Printf("Will send messages to Peer B at %s\n", remoteAddr.String())

	// Start punching to create/refresh NAT bindings
	// punch(conn, remoteAddr)

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

	// Multi-connection: create N data channels and send in parallel
	channels := 4
	if v := os.Getenv("CHANNELS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			channels = n
		}
	}
	var dcs []*webrtc.DataChannel
	dcs = make([]*webrtc.DataChannel, 0, channels)
	var metaOnce sync.Once
	readyCh := make(chan struct{}) // signaled by B after it prepares file
	var readyOnce sync.Once
	// capture sendPath early
	var sendPath string
	if len(os.Args) > 1 {
		sendPath = os.Args[1]
	}
	// Pre-validate file if provided
	var fileInfo os.FileInfo
	var fileErr error
	if sendPath != "" {
		fileInfo, fileErr = os.Stat(sendPath)
		if fileErr != nil || fileInfo.IsDir() {
			fmt.Printf("Peer A: file arg invalid: %v\n", fileErr)
			sendPath = ""
		}
	}
	for i := 0; i < channels; i++ {
		label := "p2p-" + strconv.Itoa(i)
		dc, err := pc.CreateDataChannel(label, &webrtc.DataChannelInit{ /* defaults */ })
		if err != nil {
			log.Fatalf("Peer A: create datachannel %s: %v", label, err)
		}
		dcs = append(dcs, dc)

		dc.OnOpen(func() {
			// Send metadata once when the first channel opens
			metaOnce.Do(func() {
				if sendPath == "" {
					fmt.Println("Peer A: DataChannel open -> sending greeting (no file arg)")
					_ = dc.SendText("hello from A")
					return
				}
				fi := fileInfo
				meta := map[string]interface{}{
					"name":     filepath.Base(sendPath),
					"size":     fi.Size(),
					"ts":       time.Now().Unix(),
					"ver":      2,
					"channels": channels,
				}
				metaBytes, _ := json.Marshal(meta)
				if err := dc.SendText(string(metaBytes)); err != nil {
					log.Printf("Peer A: send metadata error: %v", err)
					return
				}
				fmt.Printf("Peer A: Metadata sent. File %s (%d bytes), channels=%d\n", fi.Name(), fi.Size(), channels)
			})
		})
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				s := string(m.Data)
				if s == "READY" {
					readyOnce.Do(func() { close(readyCh) })
				} else {
					fmt.Printf("Peer A: got message on %s: %s\n", dc.Label(), s)
				}
			}
		})
	}

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
	answerStr, from, err := waitForPrefix(conn, "ANSWER:", 60*time.Second)
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

	// Start sending after READY (if any) or small timeout to avoid deadlock if receiver doesn't support READY
	go func() {
		if sendPath == "" {
			return
		}
		select {
		case <-readyCh:
			// ok
		case <-time.After(3 * time.Second):
			// proceed anyway
		}
		if err := sendFileMulti(dcs, sendPath, fileInfo.Size()); err != nil {
			log.Printf("Peer A: sendFileMulti error: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		_ = pc.Close()
	}()

	// Keep the process alive until connection finishes
	done := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer A: Connection state -> %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateDisconnected || s == webrtc.PeerConnectionStateClosed {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	})

	<-done
	fmt.Println("Peer A: exiting")
}

func punch(conn *net.UDPConn, remote *net.UDPAddr) {
	fmt.Println("Peer A: Sending punching packets...")
	for i := 0; i < 5; i++ {
		_, _ = conn.WriteToUDP([]byte("punch"), remote)
		time.Sleep(200 * time.Millisecond)
	}
	// Add extra delay to ensure peer B is ready
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
		fmt.Printf("Peer A: Ignoring UDP '%s' from %s\n", preview(msg), addr)
	}
}

func mustWriteUDP(conn *net.UDPConn, addr *net.UDPAddr, s string) {
	// Enhanced retry with exponential backoff
	for i := 0; i < 5; i++ {
		if _, err := conn.WriteToUDP([]byte(s), addr); err == nil {
			return
		}
		backoff := time.Duration(i*i*50+50) * time.Millisecond
		fmt.Printf("Peer A: UDP write retry %d after %v\n", i+1, backoff)
		time.Sleep(backoff)
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
// legacy single-channel sendFile removed in favor of sendFileMulti

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

// sendFileMulti sends the file across multiple datachannels in parallel using framed chunks.
// Frame layout: [8 bytes little-endian offset][4 bytes little-endian payload length][payload]
func sendFileMulti(dcs []*webrtc.DataChannel, filePath string, total int64) error {
	// Filter only non-nil channels
	chans := make([]*webrtc.DataChannel, 0, len(dcs))
	for _, dc := range dcs {
		if dc != nil {
			chans = append(chans, dc)
		}
	}
	if len(chans) == 0 {
		return fmt.Errorf("no datachannels available")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 2*1024*1024)
	type chunk struct {
		off uint64
		n   int
		buf []byte
	}
	chunks := make(chan chunk, 1024)
	var readErr error
	// reader goroutine
	go func() {
		var offset uint64
		for {
			buf := make([]byte, chunkSize)
			n, err := io.ReadFull(reader, buf)
			if err == io.ErrUnexpectedEOF {
				// last partial chunk
				if n > 0 {
					chunks <- chunk{off: offset, n: n, buf: buf[:n]}
					offset += uint64(n)
				}
				break
			}
			if err == io.EOF {
				break
			}
			if err != nil && err != io.ErrUnexpectedEOF {
				readErr = err
				break
			}
			chunks <- chunk{off: offset, n: n, buf: buf[:n]}
			offset += uint64(n)
		}
		close(chunks)
	}()

	// progress reporter
	var sent int64
	start := time.Now()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	stopProg := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				printSenderProgress("multi", atomic.LoadInt64(&sent), total, start)
			case <-stopProg:
				printSenderProgress("multi", atomic.LoadInt64(&sent), total, start)
				fmt.Println()
				return
			}
		}
	}()

	// per-channel backpressure helpers
	type worker struct {
		dc   *webrtc.DataChannel
		wake chan struct{}
	}
	ws := make([]worker, len(chans))
	for i, dc := range chans {
		low := uint64(maxBufferedBytes / 2)
		dc.SetBufferedAmountLowThreshold(low)
		wch := make(chan struct{}, 1)
		dc.OnBufferedAmountLow(func() {
			select {
			case wch <- struct{}{}:
			default:
			}
		})
		ws[i] = worker{dc: dc, wake: wch}
	}

	// start workers
	var wg sync.WaitGroup
	wg.Add(len(ws))
	for _, w := range ws {
		go func(w worker) {
			defer wg.Done()
			// Wait until open
			openCh := make(chan struct{})
			w.dc.OnOpen(func() { close(openCh) })
			select {
			case <-openCh:
			case <-time.After(10 * time.Second):
				// proceed anyway; channel may already be open before handler set
			}
			// consume chunks
			for c := range chunks {
				// build frame
				header := make([]byte, 12)
				binary.LittleEndian.PutUint64(header[0:8], c.off)
				binary.LittleEndian.PutUint32(header[8:12], uint32(c.n))
				// backpressure
				for w.dc.BufferedAmount() > maxBufferedBytes {
					select {
					case <-w.wake:
					case <-time.After(5 * time.Millisecond):
					}
				}
				// send header
				if err := w.dc.Send(header); err != nil {
					log.Printf("Peer A: send header error on %s: %v", w.dc.Label(), err)
					return
				}
				// send payload
				if err := w.dc.Send(c.buf[:c.n]); err != nil {
					log.Printf("Peer A: send payload error on %s: %v", w.dc.Label(), err)
					return
				}
				atomic.AddInt64(&sent, int64(c.n))
			}
		}(w)
	}
	wg.Wait()
	close(stopProg)
	if readErr != nil {
		return readErr
	}
	fmt.Println("Peer A: File sent successfully (multi).")
	return nil
}
