// quic-send reads H264 Annex B NAL units from stdin and sends them over QUIC with L4S/Prague.
// It also receives datagrams from the receiver and writes them to stdout (for benchmarking side channel).
// Usage: quic-send --addr <server:port>
package main

import (
	"encoding/binary"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kevmo314/l4s/pkg/h264"
	"github.com/kevmo314/l4s/pkg/picoquic"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5000", "Server address to connect to (host:port)")
	ccAlgo := flag.String("cc", "prague", "Congestion control algorithm (prague, bbr, cubic, newreno)")
	verbose := flag.Bool("v", false, "Verbose output")
	dataChannelTimeout := flag.Duration("dc-timeout", 5*time.Second, "Timeout to wait for data channel after sending video")
	flag.Parse()

	// Log to stderr so stdout can be used for data channel output
	log.SetOutput(os.Stderr)

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Parse address
	parts := strings.Split(*addr, ":")
	if len(parts) != 2 {
		log.Fatalf("Invalid address format: %s (expected host:port)", *addr)
	}
	host := parts[0]
	port, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil {
		log.Fatalf("Invalid port: %v", err)
	}

	if *verbose {
		log.Printf("Connecting to %s with %s congestion control", *addr, *ccAlgo)
	}

	// Create client context
	ctx, err := picoquic.NewClientContext()
	if err != nil {
		log.Fatalf("Failed to create client context: %v", err)
	}
	defer ctx.Close()

	// Set congestion control algorithm
	if err := ctx.SetCongestionAlgorithm(*ccAlgo); err != nil {
		log.Fatalf("Failed to set congestion algorithm: %v", err)
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	done := false

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		done = true
	}()

	// Connect to server
	conn, err := ctx.Connect(host, uint16(port))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	if *verbose {
		log.Printf("Connected to %s", *addr)
	}

	// Start goroutine to receive datagrams and write to stdout (data channel)
	var datagramBytesRecv int64
	var datagramCount int64
	datagramDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(datagramDone)
		buf := make([]byte, 1500)

		for {
			n, err := conn.ReadDatagram(buf)
			if err != nil {
				if *verbose {
					log.Printf("Datagram read ended: %v", err)
				}
				break
			}
			if n > 0 {
				os.Stdout.Write(buf[:n])
				atomic.AddInt64(&datagramBytesRecv, int64(n))
				atomic.AddInt64(&datagramCount, 1)
			}
		}
	}()

	// Read H264 NAL units from stdin and send them
	reader := h264.NewAnnexBReader(os.Stdin)

	nalCount := 0
	totalBytes := int64(0)
	startTime := time.Now()

	// Length prefix buffer
	lenBuf := make([]byte, 4)

	for !done {
		nal, err := reader.ReadNAL()
		if err == io.EOF {
			if *verbose {
				log.Println("End of input")
			}
			break
		}
		if err != nil {
			log.Printf("Error reading NAL: %v", err)
			break
		}

		// Send length prefix (4 bytes, big endian)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(nal.Data)))
		if _, err := conn.Write(lenBuf); err != nil {
			log.Printf("Error sending NAL length: %v", err)
			break
		}

		// Send NAL data
		if _, err := conn.Write(nal.Data); err != nil {
			log.Printf("Error sending NAL data: %v", err)
			break
		}

		nalCount++
		totalBytes += int64(len(nal.Data))

		if *verbose && nalCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			bitrate := float64(totalBytes*8) / elapsed / 1e6
			log.Printf("Sent %d NALs, %.2f MB, %.2f Mbps", nalCount, float64(totalBytes)/1e6, bitrate)
		}
	}

	// Send FIN to signal end of stream
	if err := conn.FinishStream(); err != nil {
		log.Printf("Error finishing stream: %v", err)
	} else if *verbose {
		log.Println("Sent stream FIN")
	}

	elapsed := time.Since(startTime)
	log.Printf("Finished: sent %d NALs, %d bytes in %v", nalCount, totalBytes, elapsed)

	if totalBytes > 0 && elapsed.Seconds() > 0 {
		bitrate := float64(totalBytes*8) / elapsed.Seconds() / 1e6
		log.Printf("Average bitrate: %.2f Mbps", bitrate)
	}

	// Wait for data channel with timeout
	if *verbose {
		log.Printf("Waiting %v for data channel...", *dataChannelTimeout)
	}
	select {
	case <-datagramDone:
		if *verbose {
			log.Println("Data channel closed")
		}
	case <-time.After(*dataChannelTimeout):
		if *verbose {
			log.Println("Data channel timeout")
		}
	}

	// Print data channel stats
	dgBytes := atomic.LoadInt64(&datagramBytesRecv)
	dgCount := atomic.LoadInt64(&datagramCount)
	if dgCount > 0 {
		log.Printf("Data channel: received %d datagrams, %d bytes", dgCount, dgBytes)
		totalElapsed := time.Since(startTime)
		if totalElapsed.Seconds() > 0 {
			dgBitrate := float64(dgBytes*8) / totalElapsed.Seconds() / 1e6
			log.Printf("Data channel bitrate: %.2f Mbps", dgBitrate)
		}
	}
}
