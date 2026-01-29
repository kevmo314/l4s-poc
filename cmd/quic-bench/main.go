// quic-bench is a simple QUIC throughput test without video processing
package main

import (
	"flag"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/kevmo314/l4s/pkg/picoquic"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5000", "Server address (host:port)")
	ccAlgo := flag.String("cc", "prague", "Congestion control algorithm")
	duration := flag.Duration("duration", 10*time.Second, "Test duration")
	chunkSize := flag.Int("chunk", 16384, "Chunk size in bytes")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	parts := strings.Split(*addr, ":")
	if len(parts) != 2 {
		log.Fatalf("Invalid address: %s", *addr)
	}
	host := parts[0]
	port, _ := strconv.ParseUint(parts[1], 10, 16)

	log.Printf("Connecting to %s with %s CC, chunk=%d bytes", *addr, *ccAlgo, *chunkSize)

	ctx, err := picoquic.NewClientContext()
	if err != nil {
		log.Fatalf("Failed to create context: %v", err)
	}
	defer ctx.Close()

	if err := ctx.SetCongestionAlgorithm(*ccAlgo); err != nil {
		log.Fatalf("Failed to set CC: %v", err)
	}

	conn, err := ctx.Connect(host, uint16(port))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	log.Println("Connected, starting throughput test")

	// Create chunk of random data
	chunk := make([]byte, *chunkSize)
	for i := range chunk {
		chunk[i] = byte(i % 256)
	}

	var totalBytes int64
	start := time.Now()
	deadline := start.Add(*duration)
	lastLog := start

	for time.Now().Before(deadline) {
		n, err := conn.Write(chunk)
		if err != nil {
			log.Printf("Write error: %v", err)
			break
		}
		totalBytes += int64(n)

		// Log every second
		if time.Since(lastLog) > time.Second {
			elapsed := time.Since(start).Seconds()
			mbps := float64(totalBytes*8) / elapsed / 1e6
			log.Printf("Sent %d bytes, %.2f Mbps", totalBytes, mbps)
			lastLog = time.Now()
		}
	}

	// Send FIN and wait
	if err := conn.FinishStream(); err != nil {
		log.Printf("FinishStream error: %v", err)
	}

	log.Println("Waiting for acknowledgement...")
	if err := conn.WaitStreamComplete(30000); err != nil {
		log.Printf("WaitStreamComplete: %v", err)
	}

	elapsed := time.Since(start)
	mbps := float64(totalBytes*8) / elapsed.Seconds() / 1e6
	log.Printf("Final: sent %d bytes in %v = %.2f Mbps", totalBytes, elapsed, mbps)
}
