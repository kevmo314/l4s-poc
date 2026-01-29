// webrtc-recv receives H264 video via WebRTC and writes NAL units to stdout.
// It also reads from stdin and sends data back to the sender via DataChannel (for benchmarking side channel).
// Usage: webrtc-recv --http-addr :8080
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kevmo314/l4s/pkg/h264"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
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

// getGCPExternalIP queries the GCP metadata server for the instance's external IP
func getGCPExternalIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://metadata.google.internal/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip",
		nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(ip))
}

func main() {
	httpAddr := flag.String("http-addr", ":8080", "HTTP address for WHIP endpoint")
	whipPath := flag.String("whip-path", "/whip", "WHIP endpoint path")
	nat1to1IP := flag.String("nat-1to1-ip", "", "Public IP for 1:1 NAT mapping (auto-detects GCP if empty)")
	verbose := flag.Bool("v", false, "Verbose output")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Auto-detect public IP from GCP metadata if not explicitly set
	publicIP := *nat1to1IP
	if publicIP == "" {
		if gcpIP := getGCPExternalIP(); gcpIP != "" {
			publicIP = gcpIP
			log.Printf("Auto-detected GCP external IP: %s", publicIP)
		}
	}

	if *verbose {
		log.Printf("Starting WebRTC receiver on %s%s", *httpAddr, *whipPath)
	}

	// Output writer (stdout)
	writer := h264.NewAnnexBWriter(os.Stdout)

	// Create HTTP server with WHIP endpoint
	http.HandleFunc(*whipPath, func(w http.ResponseWriter, r *http.Request) {
		// Reset stats for each new connection
		var (
			nalCount    int
			totalBytes  int64
			startTime   time.Time
			statsMu     sync.Mutex
			firstPacket bool = true
		)
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read SDP offer
		offer, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read offer", http.StatusBadRequest)
			return
		}

		if *verbose {
			log.Printf("Received WHIP offer (%d bytes)", len(offer))
		}

		// Create media engine and register codecs
		m := &webrtc.MediaEngine{}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
			},
			PayloadType: 96,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			http.Error(w, "Failed to register codec", http.StatusInternalServerError)
			return
		}

		// Create interceptor registry with PLI support
		i := &interceptor.Registry{}
		intervalPLI, err := intervalpli.NewReceiverInterceptor()
		if err != nil {
			http.Error(w, "Failed to create PLI interceptor", http.StatusInternalServerError)
			return
		}
		i.Add(intervalPLI)

		// Create setting engine for NAT configuration
		se := webrtc.SettingEngine{}
		if publicIP != "" {
			se.SetNAT1To1IPs([]string{publicIP}, webrtc.ICECandidateTypeHost)
			// Use port range that's typically allowed through cloud firewalls
			se.SetEphemeralUDPPortRange(10000, 60000)
			if *verbose {
				log.Printf("Configured NAT 1:1 mapping to %s with UDP ports 10000-60000", publicIP)
			}
		}

		// Create API with media engine, interceptors, and settings
		api := webrtc.NewAPI(
			webrtc.WithMediaEngine(m),
			webrtc.WithInterceptorRegistry(i),
			webrtc.WithSettingEngine(se),
		)

		// Create PeerConnection configuration
		config := webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{URLs: []string{"stun:stun.l.google.com:19302"}},
			},
		}

		// Create PeerConnection
		pc, err := api.NewPeerConnection(config)
		if err != nil {
			http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
			return
		}

		// Create DataChannel for side channel (receiver → sender)
		var dataChannelBytesSent int64
		var dataChannelMsgCount int64

		dc, err := pc.CreateDataChannel("side-channel", nil)
		if err != nil {
			http.Error(w, "Failed to create data channel", http.StatusInternalServerError)
			return
		}

		dc.OnOpen(func() {
			if *verbose {
				log.Println("DataChannel opened, starting stats sender")
			}

			// Send bandwidth stats every 10ms for latency measurement
			go func() {
				interval := 10 * time.Millisecond
				ticker := time.NewTicker(interval)
				defer ticker.Stop()

				var seq uint64

				if *verbose {
					log.Printf("DataChannel sending bandwidth stats every %v", interval)
				}

				for range ticker.C {
					seq++

					// Get current stats
					statsMu.Lock()
					currentBytes := totalBytes
					currentNALs := nalCount
					var elapsedMs int64
					var bitrateMbps float64
					if !firstPacket {
						elapsed := time.Since(startTime)
						elapsedMs = elapsed.Milliseconds()
						if elapsed.Seconds() > 0 {
							bitrateMbps = float64(currentBytes*8) / elapsed.Seconds() / 1e6
						}
					}
					statsMu.Unlock()

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

					if err := dc.Send(data); err != nil {
						if *verbose {
							log.Printf("Error sending on DataChannel: %v", err)
						}
						break
					}
					atomic.AddInt64(&dataChannelBytesSent, int64(len(data)))
					atomic.AddInt64(&dataChannelMsgCount, 1)
				}
				if *verbose {
					log.Printf("Stats sender finished: sent %d messages, %d bytes",
						atomic.LoadInt64(&dataChannelMsgCount), atomic.LoadInt64(&dataChannelBytesSent))
				}
			}()
		})

		dc.OnClose(func() {
			if *verbose {
				log.Println("DataChannel closed")
			}
		})

		// Handle track
		pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			if *verbose {
				log.Printf("Received track: codec=%s", track.Codec().MimeType)
			}

			// Create H264 depacketizer
			depacketizer := &codecs.H264Packet{}

			// Buffer for accumulating fragmented NAL units
			var nalBuffer []byte

			for {
				// Read RTP packet
				rtpPacket, _, err := track.ReadRTP()
				if err != nil {
					if err == io.EOF {
						break
					}
					log.Printf("Error reading RTP: %v", err)
					break
				}

				// Track first packet time
				statsMu.Lock()
				if firstPacket {
					startTime = time.Now()
					firstPacket = false
				}
				statsMu.Unlock()

				// Depacketize
				nalData, err := depacketizer.Unmarshal(rtpPacket.Payload)
				if err != nil {
					log.Printf("Error depacketizing: %v", err)
					continue
				}

				// Accumulate NAL data
				nalBuffer = append(nalBuffer, nalData...)

				// On marker bit (end of NAL), write to output
				if rtpPacket.Marker {
					if len(nalBuffer) > 0 {
						nal := &h264.NALUnit{Data: nalBuffer}
						if err := writer.WriteNAL(nal); err != nil {
							log.Printf("Error writing NAL: %v", err)
						}

						statsMu.Lock()
						nalCount++
						totalBytes += int64(len(nalBuffer))

						if *verbose && nalCount%100 == 0 {
							elapsed := time.Since(startTime).Seconds()
							bitrate := float64(totalBytes*8) / elapsed / 1e6
							log.Printf("Received %d NALs, %.2f MB, %.2f Mbps", nalCount, float64(totalBytes)/1e6, bitrate)
						}
						statsMu.Unlock()

						nalBuffer = nil
					}
				}
			}

			// Flush any remaining NAL data
			if len(nalBuffer) > 0 {
				nal := &h264.NALUnit{Data: nalBuffer}
				if err := writer.WriteNAL(nal); err != nil {
					log.Printf("Error writing final NAL: %v", err)
				}
				statsMu.Lock()
				nalCount++
				totalBytes += int64(len(nalBuffer))
				statsMu.Unlock()
				nalBuffer = nil
			}

			// Print final stats
			statsMu.Lock()
			elapsed := time.Since(startTime)
			log.Printf("Finished: received %d NALs, %d bytes in %v", nalCount, totalBytes, elapsed)
			if totalBytes > 0 && elapsed.Seconds() > 0 {
				bitrate := float64(totalBytes*8) / elapsed.Seconds() / 1e6
				log.Printf("Average bitrate: %.2f Mbps", bitrate)
			}
			statsMu.Unlock()

			// Print DataChannel stats
			dcBytes := atomic.LoadInt64(&dataChannelBytesSent)
			dcCount := atomic.LoadInt64(&dataChannelMsgCount)
			if dcCount > 0 {
				log.Printf("Data channel: sent %d messages, %d bytes", dcCount, dcBytes)
				if elapsed.Seconds() > 0 {
					dcBitrate := float64(dcBytes*8) / elapsed.Seconds() / 1e6
					log.Printf("Data channel bitrate: %.2f Mbps", dcBitrate)
				}
			}
		})

		// Set the remote description (offer)
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  string(offer),
		}); err != nil {
			http.Error(w, "Failed to set remote description", http.StatusBadRequest)
			return
		}

		// Create answer
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			http.Error(w, "Failed to create answer", http.StatusInternalServerError)
			return
		}

		// Start gathering ICE candidates
		gatherComplete := webrtc.GatheringCompletePromise(pc)

		// Set local description
		if err := pc.SetLocalDescription(answer); err != nil {
			http.Error(w, "Failed to set local description", http.StatusInternalServerError)
			return
		}

		// Wait for ICE gathering to complete
		<-gatherComplete

		if *verbose {
			log.Printf("ICE gathering complete, sending answer")
		}

		// Return the answer
		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", *whipPath)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(pc.LocalDescription().SDP))
	})

	log.Printf("Listening on %s%s", *httpAddr, *whipPath)
	if err := http.ListenAndServe(*httpAddr, nil); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
