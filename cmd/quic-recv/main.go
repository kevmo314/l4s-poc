// quic-recv receives H264 NAL units over QUIC with L4S/Prague and writes them to stdout as Annex B.
// It also reads from stdin and sends data back to the sender via datagrams (for benchmarking side channel).
// Usage: quic-recv --listen <port> --cert <cert.pem> --key <key.pem>
package main

import (
	"encoding/binary"
	"encoding/json"
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

// BandwidthStats is sent over the datachannel to report receiver bandwidth
type BandwidthStats struct {
	Seq         uint64  `json:"seq"`     // Sequence number for loss/reorder detection
	Timestamp   int64   `json:"ts"`      // Unix timestamp in ms (for latency measurement)
	BytesRecv   int64   `json:"bytes"`   // Total bytes received
	BitrateKbps float64 `json:"kbps"`    // Current bitrate in kbps
	BitrateMbps float64 `json:"mbps"`    // Current bitrate in Mbps
	NALCount    int     `json:"nals"`    // Total NALs received
	ElapsedMs   int64   `json:"elapsed"` // Elapsed time in ms
}

func main() {
	port := flag.Uint("port", 5000, "Port to listen on")
	certFile := flag.String("cert", "certs/cert.pem", "TLS certificate file")
	keyFile := flag.String("key", "certs/key.pem", "TLS key file")
	ccAlgo := flag.String("cc", "prague", "Congestion control algorithm (prague, bbr, cubic, newreno)")
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

	// Stats variables (atomic for goroutine access)
	var nalCountAtomic int64
	var totalBytesAtomic int64
	startTime := time.Now()

	// Start goroutine to send bandwidth stats via datagrams (side channel)
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

		// Send stats every 10ms for latency measurement
		interval := 10 * time.Millisecond
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		var seq uint64

		if *verbose {
			log.Printf("Data channel sending bandwidth stats every %v", interval)
		}

		for {
			select {
			case <-datagramStop:
				return
			case <-ticker.C:
				seq++

				// Get current stats
				currentBytes := atomic.LoadInt64(&totalBytesAtomic)
				currentNALs := int(atomic.LoadInt64(&nalCountAtomic))
				elapsed := time.Since(startTime)
				elapsedMs := elapsed.Milliseconds()
				var bitrateMbps float64
				if elapsed.Seconds() > 0 {
					bitrateMbps = float64(currentBytes*8) / elapsed.Seconds() / 1e6
				}

				stats := BandwidthStats{
					Seq:         seq,
					Timestamp:   time.Now().UnixMilli(),
					BytesRecv:   currentBytes,
					BitrateKbps: bitrateMbps * 1000,
					BitrateMbps: bitrateMbps,
					NALCount:    currentNALs,
					ElapsedMs:   elapsedMs,
				}

				data, err := json.Marshal(stats)
				if err != nil {
					log.Printf("Error marshaling stats: %v", err)
					continue
				}

				if _, err := conn.WriteDatagram(data); err != nil {
					if *verbose {
						log.Printf("Error sending datagram: %v", err)
					}
					return
				}
				atomic.AddInt64(&datagramBytesSent, int64(len(data)))
				atomic.AddInt64(&datagramCount, 1)
			}
		}
	}()

	// Create H264 Annex B writer for stdout
	annexBWriter := h264.NewAnnexBWriter(os.Stdout)

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

		newNALCount := atomic.AddInt64(&nalCountAtomic, 1)
		newTotalBytes := atomic.AddInt64(&totalBytesAtomic, int64(nalLen))

		// Signal that first NAL was received (enables data channel)
		if newNALCount == 1 {
			firstNALOnce.Do(func() {
				close(firstNALReceived)
			})
		}

		if *verbose && newNALCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			bitrate := float64(newTotalBytes*8) / elapsed / 1e6
			log.Printf("Received %d NALs, %.2f MB, %.2f Mbps", newNALCount, float64(newTotalBytes)/1e6, bitrate)
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
	finalNALCount := atomic.LoadInt64(&nalCountAtomic)
	finalTotalBytes := atomic.LoadInt64(&totalBytesAtomic)
	log.Printf("Finished: received %d NALs, %d bytes in %v", finalNALCount, finalTotalBytes, elapsed)

	if finalTotalBytes > 0 && elapsed.Seconds() > 0 {
		bitrate := float64(finalTotalBytes*8) / elapsed.Seconds() / 1e6
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
