// quic-recv receives H264 NAL units over QUIC with L4S/Prague and writes them to stdout as Annex B.
// It also reads from stdin and sends data back to the sender via datagrams (for benchmarking side channel).
// Usage: quic-recv --listen <port> --cert <cert.pem> --key <key.pem>
package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kevmo314/l4s/pkg/h264"
	"github.com/kevmo314/l4s/pkg/picoquic"
)

func main() {
	port := flag.Uint("port", 5000, "Port to listen on")
	certFile := flag.String("cert", "certs/cert.pem", "TLS certificate file")
	keyFile := flag.String("key", "certs/key.pem", "TLS key file")
	ccAlgo := flag.String("cc", "prague", "Congestion control algorithm (prague, bbr, cubic, newreno)")
	dcBitrate := flag.Int("dc-bitrate", 100, "Data channel send bitrate in kbps (0 to disable)")
	verbose := flag.Bool("v", false, "Verbose output to stderr")
	flag.Parse()

	// Log to stderr so stdout can be used for H264 output
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if *verbose {
		log.Printf("Starting QUIC receiver on port %d with %s congestion control", *port, *ccAlgo)
	}

	// Create server context
	ctx, err := picoquic.NewServerContext(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("Failed to create server context: %v", err)
	}
	defer ctx.Close()

	// Set congestion control algorithm
	if err := ctx.SetCongestionAlgorithm(*ccAlgo); err != nil {
		log.Fatalf("Failed to set congestion algorithm: %v", err)
	}

	// Start listening
	if err := ctx.Listen(uint16(*port)); err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	if *verbose {
		log.Printf("Listening on port %d...", *port)
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Accept a connection in a goroutine so we can handle signals
	connChan := make(chan *picoquic.Connection, 1)
	errChan := make(chan error, 1)

	go func() {
		conn, err := ctx.Accept()
		if err != nil {
			errChan <- err
			return
		}
		connChan <- conn
	}()

	if *verbose {
		log.Println("Waiting for connection...")
	}

	var conn *picoquic.Connection
	select {
	case <-sigChan:
		log.Println("Received shutdown signal before connection")
		return
	case err := <-errChan:
		log.Fatalf("Failed to accept connection: %v", err)
	case conn = <-connChan:
		// Connected
	}
	defer conn.Close()

	if *verbose {
		log.Println("Connection established")
	}

	// Channel to signal when first NAL is received (enables data channel)
	firstNALReceived := make(chan struct{})
	var firstNALOnce sync.Once

	// Start goroutine to send datagrams at consistent bitrate (side channel)
	var datagramBytesSent int64
	var datagramCount int64
	datagramStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Wait for first NAL to be received before sending datagrams
		// This ensures the stream is properly established first
		<-firstNALReceived

		if *dcBitrate <= 0 {
			if *verbose {
				log.Println("Data channel bitrate is 0, not sending data")
			}
			return
		}

		// Send every 100ms (10 times per second)
		interval := 100 * time.Millisecond
		bytesPerInterval := (*dcBitrate * 1000 / 8) / 10 // kbps to bytes per 100ms

		buf := make([]byte, bytesPerInterval)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		if *verbose {
			log.Printf("Data channel sending %d bytes every %v (%d kbps)", bytesPerInterval, interval, *dcBitrate)
		}

		for {
			select {
			case <-datagramStop:
				return
			case <-ticker.C:
				// Fill with random data
				if _, err := rand.Read(buf); err != nil {
					log.Printf("Error generating random data: %v", err)
					return
				}

				if _, err := conn.WriteDatagram(buf); err != nil {
					if *verbose {
						log.Printf("Error sending datagram: %v", err)
					}
					return
				}
				atomic.AddInt64(&datagramBytesSent, int64(len(buf)))
				atomic.AddInt64(&datagramCount, 1)
			}
		}
	}()

	// Create H264 Annex B writer for stdout
	annexBWriter := h264.NewAnnexBWriter(os.Stdout)

	nalCount := 0
	totalBytes := int64(0)
	startTime := time.Now()

	// Read loop
	lenBuf := make([]byte, 4)
	done := false

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		done = true
		conn.Close()
	}()

	for !done {
		// Read length prefix (4 bytes, big endian)
		if _, err := io.ReadFull(&connReader{conn}, lenBuf); err != nil {
			if err == io.EOF {
				if *verbose {
					log.Println("End of stream")
				}
				break
			}
			if !done {
				log.Printf("Error reading NAL length: %v", err)
			}
			break
		}

		nalLen := binary.BigEndian.Uint32(lenBuf)
		if nalLen == 0 || nalLen > 10*1024*1024 {
			log.Printf("Invalid NAL length: %d", nalLen)
			break
		}

		// Read NAL data
		nalData := make([]byte, nalLen)
		if _, err := io.ReadFull(&connReader{conn}, nalData); err != nil {
			if !done {
				log.Printf("Error reading NAL data: %v", err)
			}
			break
		}

		// Create NAL unit
		nal := &h264.NALUnit{
			Type: nalData[0] & 0x1F,
			Data: nalData,
		}

		// Write to stdout in Annex B format
		if err := annexBWriter.WriteNAL(nal); err != nil {
			log.Printf("Error writing NAL: %v", err)
			break
		}

		nalCount++
		totalBytes += int64(nalLen)

		// Signal that first NAL was received (enables data channel)
		if nalCount == 1 {
			firstNALOnce.Do(func() {
				close(firstNALReceived)
			})
		}

		if *verbose && nalCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			bitrate := float64(totalBytes*8) / elapsed / 1e6
			log.Printf("Received %d NALs, %.2f MB, %.2f Mbps", nalCount, float64(totalBytes)/1e6, bitrate)
		}
	}

	// Close the firstNALReceived channel if it wasn't already (in case no NALs were received)
	firstNALOnce.Do(func() {
		close(firstNALReceived)
	})

	// Stop the datagram sender
	close(datagramStop)
	wg.Wait()

	elapsed := time.Since(startTime)
	log.Printf("Finished: received %d NALs, %d bytes in %v", nalCount, totalBytes, elapsed)

	if totalBytes > 0 && elapsed.Seconds() > 0 {
		bitrate := float64(totalBytes*8) / elapsed.Seconds() / 1e6
		log.Printf("Average bitrate: %.2f Mbps", bitrate)
	}

	// Print data channel stats
	dgBytes := atomic.LoadInt64(&datagramBytesSent)
	dgCount := atomic.LoadInt64(&datagramCount)
	if dgCount > 0 {
		log.Printf("Data channel: sent %d datagrams, %d bytes", dgCount, dgBytes)
		if elapsed.Seconds() > 0 {
			dgBitrate := float64(dgBytes*8) / elapsed.Seconds() / 1e6
			log.Printf("Data channel bitrate: %.2f Mbps", dgBitrate)
		}
	}
}

// connReader wraps picoquic.Connection to implement io.Reader
type connReader struct {
	conn *picoquic.Connection
}

func (r *connReader) Read(p []byte) (int, error) {
	return r.conn.Read(p)
}
