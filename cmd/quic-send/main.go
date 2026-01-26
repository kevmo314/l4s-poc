// quic-send reads H264 Annex B NAL units from stdin and sends them over QUIC with L4S/Prague.
// It also receives datagrams from the receiver and writes them to stdout (for benchmarking side channel).
// Usage: quic-send --addr <server:port>
package main

import (
	"encoding/binary"
	"encoding/json"
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

// BandwidthStats is received from the receiver over the datachannel
type BandwidthStats struct {
	Seq         uint64  `json:"seq"`     // Sequence number for loss/reorder detection
	Timestamp   int64   `json:"ts"`      // Unix timestamp in ms (for latency measurement)
	BytesRecv   int64   `json:"bytes"`   // Total bytes received
	BitrateKbps float64 `json:"kbps"`    // Current bitrate in kbps
	BitrateMbps float64 `json:"mbps"`    // Current bitrate in Mbps
	NALCount    int     `json:"nals"`    // Total NALs received
	ElapsedMs   int64   `json:"elapsed"` // Elapsed time in ms
}

// Summary is the final stats output to stdout as JSON
type Summary struct {
	// Sender stats
	SenderNALs      int     `json:"sender_nals"`
	SenderBytes     int64   `json:"sender_bytes"`
	SenderBitrate   float64 `json:"sender_mbps"`
	SenderElapsedMs int64   `json:"sender_elapsed_ms"`

	// Receiver stats (from datachannel)
	ReceiverNALs    int     `json:"receiver_nals"`
	ReceiverBytes   int64   `json:"receiver_bytes"`
	ReceiverBitrate float64 `json:"receiver_mbps"`

	// Latency stats (ms)
	LatencyAvg     float64 `json:"latency_avg_ms"`
	LatencyMin     int64   `json:"latency_min_ms"`
	LatencyMax     int64   `json:"latency_max_ms"`
	LatencySamples int64   `json:"latency_samples"`

	// Packet stats
	PacketsLost      int64 `json:"packets_lost"`
	PacketsReordered int64 `json:"packets_reordered"`
}

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
	var datagramOpen int32
	datagramDone := make(chan struct{})
	datagramStartTime := time.Now()

	// Store last received bandwidth stats from receiver
	var lastRecvStats BandwidthStats
	var lastRecvStatsMu sync.Mutex

	// Latency tracking (note: one-way latency may be negative due to clock skew)
	var latencySum int64
	var latencyCount int64
	var latencyMin int64 = 999999999
	var latencyMax int64 = -999999999
	var lastSeq uint64
	var packetsLost int64
	var packetsReordered int64

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(datagramDone)
		buf := make([]byte, 1500)
		first := true

		for {
			n, err := conn.ReadDatagram(buf)
			if err != nil {
				if *verbose {
					log.Printf("Datagram read ended: %v", err)
				}
				break
			}
			if n > 0 {
				recvTime := time.Now().UnixMilli()
				if first {
					atomic.StoreInt32(&datagramOpen, 1)
					datagramStartTime = time.Now()
					first = false
					if *verbose {
						log.Println("Data channel active (first datagram received)")
					}
				}
				atomic.AddInt64(&datagramBytesRecv, int64(n))
				newCount := atomic.AddInt64(&datagramCount, 1)

				// Parse bandwidth stats from receiver
				var stats BandwidthStats
				if err := json.Unmarshal(buf[:n], &stats); err == nil {
					lastRecvStatsMu.Lock()
					lastRecvStats = stats

					// Calculate one-way latency (may be negative due to clock skew)
					latency := recvTime - stats.Timestamp
					latencySum += latency
					latencyCount++
					if latency < latencyMin {
						latencyMin = latency
					}
					if latency > latencyMax {
						latencyMax = latency
					}

					// Check for packet loss/reordering
					if stats.Seq > 0 {
						if lastSeq > 0 {
							if stats.Seq > lastSeq+1 {
								packetsLost += int64(stats.Seq - lastSeq - 1)
							} else if stats.Seq <= lastSeq {
								packetsReordered++
							}
						}
						lastSeq = stats.Seq
					}
					lastRecvStatsMu.Unlock()

					if *verbose && newCount%100 == 1 {
						avgLatency := float64(latencySum) / float64(latencyCount)
						log.Printf("Receiver: %.2f Mbps | Latency: %.1fms avg, %dms min, %dms max | Lost: %d, Reorder: %d",
							stats.BitrateMbps, avgLatency, latencyMin, latencyMax, packetsLost, packetsReordered)
					}
				}
			}
		}
	}()

	// Start data channel stats logger
	statsStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		var lastBytes int64
		for {
			select {
			case <-statsStop:
				return
			case <-ticker.C:
				if atomic.LoadInt32(&datagramOpen) == 1 {
					currentBytes := atomic.LoadInt64(&datagramBytesRecv)
					currentCount := atomic.LoadInt64(&datagramCount)
					elapsed := time.Since(datagramStartTime).Seconds()
					instantBitrate := float64(currentBytes-lastBytes) * 8 / 1e3 // kbps in last second
					avgBitrate := float64(currentBytes*8) / elapsed / 1e3       // kbps average
					if *verbose {
						log.Printf("DataChannel: %d msgs, %d bytes, instant=%.1f kbps, avg=%.1f kbps",
							currentCount, currentBytes, instantBitrate, avgBitrate)
					}
					lastBytes = currentBytes
				}
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

	close(statsStop)
	elapsed := time.Since(startTime)
	log.Printf("Finished: sent %d NALs, %d bytes in %v", nalCount, totalBytes, elapsed)

	var senderBitrate float64
	if totalBytes > 0 && elapsed.Seconds() > 0 {
		senderBitrate = float64(totalBytes*8) / elapsed.Seconds() / 1e6
		log.Printf("Average bitrate: %.2f Mbps", senderBitrate)
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

	// Build and output summary
	lastRecvStatsMu.Lock()
	finalStats := lastRecvStats
	finalLatencyCount := latencyCount
	finalLatencySum := latencySum
	finalLatencyMin := latencyMin
	finalLatencyMax := latencyMax
	finalPacketsLost := packetsLost
	finalPacketsReordered := packetsReordered
	lastRecvStatsMu.Unlock()

	var avgLatency float64
	if finalLatencyCount > 0 {
		avgLatency = float64(finalLatencySum) / float64(finalLatencyCount)
	}

	summary := Summary{
		SenderNALs:       nalCount,
		SenderBytes:      totalBytes,
		SenderBitrate:    senderBitrate,
		SenderElapsedMs:  elapsed.Milliseconds(),
		ReceiverNALs:     finalStats.NALCount,
		ReceiverBytes:    finalStats.BytesRecv,
		ReceiverBitrate:  finalStats.BitrateMbps,
		LatencyAvg:       avgLatency,
		LatencyMin:       finalLatencyMin,
		LatencyMax:       finalLatencyMax,
		LatencySamples:   finalLatencyCount,
		PacketsLost:      finalPacketsLost,
		PacketsReordered: finalPacketsReordered,
	}

	// Output JSON summary to stdout
	if err := json.NewEncoder(os.Stdout).Encode(summary); err != nil {
		log.Printf("Error encoding summary: %v", err)
	}

	// Also log to stderr for visibility
	dgCount := atomic.LoadInt64(&datagramCount)
	if dgCount > 0 {
		if finalStats.BytesRecv > 0 {
			log.Printf("Receiver reported: %.2f Mbps (%d NALs, %d bytes)",
				finalStats.BitrateMbps, finalStats.NALCount, finalStats.BytesRecv)
		}
		if finalLatencyCount > 0 {
			log.Printf("Latency: %.1fms avg, %dms min, %dms max (%d samples)",
				avgLatency, finalLatencyMin, finalLatencyMax, finalLatencyCount)
		}
	}
}
