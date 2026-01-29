// Minimal QUIC throughput test - runs server and client locally
package main

import (
	"flag"
	"log"
	"sync"
	"time"

	"github.com/kevmo314/l4s/pkg/picoquic"
)

func main() {
	ccAlgo := flag.String("cc", "prague", "Congestion control algorithm")
	duration := flag.Duration("d", 5*time.Second, "Test duration")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Testing with %s CC for %v", *ccAlgo, *duration)

	var wg sync.WaitGroup
	serverReady := make(chan struct{})

	// Start server
	wg.Add(1)
	go func() {
		defer wg.Done()
		runServer(serverReady)
	}()

	<-serverReady
	time.Sleep(100 * time.Millisecond)

	// Run client
	runClient(*ccAlgo, *duration)

	wg.Wait()
}

func runServer(ready chan struct{}) {
	ctx, err := picoquic.NewServerContext("certs/cert.pem", "certs/key.pem")
	if err != nil {
		log.Fatalf("Server: failed to create context: %v", err)
	}
	defer ctx.Close()

	if err := ctx.Listen(29999); err != nil {
		log.Fatalf("Server: failed to listen: %v", err)
	}

	log.Println("Server: listening on port 29999")
	close(ready)

	conn, err := ctx.Accept()
	if err != nil {
		log.Fatalf("Server: failed to accept: %v", err)
	}
	defer conn.Close()

	log.Println("Server: connection accepted")

	// Read and discard data
	buf := make([]byte, 65536)
	var totalBytes int64
	start := time.Now()

	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Printf("Server: read ended: %v", err)
			break
		}
		totalBytes += int64(n)
	}

	elapsed := time.Since(start)
	mbps := float64(totalBytes*8) / elapsed.Seconds() / 1e6
	log.Printf("Server: received %d bytes in %v = %.2f Mbps", totalBytes, elapsed, mbps)
}

func runClient(ccAlgo string, duration time.Duration) {
	ctx, err := picoquic.NewClientContext()
	if err != nil {
		log.Fatalf("Client: failed to create context: %v", err)
	}
	defer ctx.Close()

	if err := ctx.SetCongestionAlgorithm(ccAlgo); err != nil {
		log.Fatalf("Client: failed to set CC: %v", err)
	}

	conn, err := ctx.Connect("127.0.0.1", 29999)
	if err != nil {
		log.Fatalf("Client: failed to connect: %v", err)
	}
	defer conn.Close()

	log.Println("Client: connected")

	// Send data
	chunk := make([]byte, 16384)
	var totalBytes int64
	start := time.Now()
	deadline := start.Add(duration)
	lastLog := start

	for time.Now().Before(deadline) {
		n, err := conn.Write(chunk)
		if err != nil {
			log.Printf("Client: write error: %v", err)
			break
		}
		totalBytes += int64(n)

		if time.Since(lastLog) > time.Second {
			elapsed := time.Since(start).Seconds()
			mbps := float64(totalBytes*8) / elapsed / 1e6
			log.Printf("Client: sent %d bytes, %.2f Mbps (queued)", totalBytes, mbps)
			lastLog = time.Now()
		}
	}

	conn.FinishStream()
	log.Println("Client: waiting for ACKs...")
	conn.WaitStreamComplete(10000)

	elapsed := time.Since(start)
	mbps := float64(totalBytes*8) / elapsed.Seconds() / 1e6
	log.Printf("Client: total %d bytes in %v = %.2f Mbps", totalBytes, elapsed, mbps)
}
