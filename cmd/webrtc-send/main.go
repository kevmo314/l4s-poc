// webrtc-send reads H264 NAL units from stdin and sends them via WebRTC.
// It also receives data from the receiver via DataChannel and writes it to stdout (for benchmarking side channel).
// Usage: webrtc-send --whip-url http://receiver:8080/whip
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kevmo314/l4s/pkg/h264"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/webrtc/v4"
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
	SenderBytes     int64   `json:"sender_bytes"`
	SenderBitrate   float64 `json:"sender_mbps"`
	SenderElapsedMs int64   `json:"sender_elapsed_ms"`

	// Receiver stats (from datachannel)
	ReceiverBytes   int64   `json:"receiver_bytes"`
	ReceiverBitrate float64 `json:"receiver_mbps"`

	// DataChannel stats (return path from receiver)
	DataChannelMbps float64 `json:"datachannel_mbps"`

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
	whipURL := flag.String("whip-url", "http://127.0.0.1:8080/whip", "WHIP endpoint URL")
	verbose := flag.Bool("v", false, "Verbose output")
	framerate := flag.Int("fps", 30, "Frame rate for timestamp calculation")
	dataChannelTimeout := flag.Duration("dc-timeout", 5*time.Second, "Timeout to wait for data channel after sending video")
	nat1to1IP := flag.String("nat-1to1-ip", "", "Public IP for 1:1 NAT mapping (for Docker/NAT environments)")
	turnServer := flag.String("turn-server", "", "TURN server URL (e.g., turn:turn.example.com:3478)")
	turnUser := flag.String("turn-user", "", "TURN server username")
	turnPass := flag.String("turn-pass", "", "TURN server password")
	initialBitrate := flag.Int("bitrate", 20_000_000, "Initial target bitrate in bps for congestion control")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if *verbose {
		log.Printf("Connecting to WHIP endpoint: %s", *whipURL)
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
		log.Fatalf("Failed to register codec: %v", err)
	}

	// Create interceptor registry with GCC for sender-side congestion control
	i := &interceptor.Registry{}

	// Add Google Congestion Controller for sender-side bandwidth estimation and pacing
	congestionController, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(*initialBitrate),
			gcc.SendSideBWEMaxBitrate(100_000_000), // 100 Mbps max
			gcc.SendSideBWEMinBitrate(100_000),     // 100 Kbps min
		)
	})
	if err != nil {
		log.Fatalf("Failed to create congestion controller: %v", err)
	}
	i.Add(congestionController)

	// Configure TWCC sender for congestion control feedback
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, i); err != nil {
		log.Fatalf("Failed to configure TWCC: %v", err)
	}

	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		log.Fatalf("Failed to register interceptors: %v", err)
	}

	if *verbose {
		log.Printf("Congestion control enabled with initial bitrate %d bps", *initialBitrate)
	}

	// Create setting engine for NAT configuration
	se := webrtc.SettingEngine{}
	if *nat1to1IP != "" {
		se.SetNAT1To1IPs([]string{*nat1to1IP}, webrtc.ICECandidateTypeHost)
		// Use port range that's typically allowed through firewalls
		se.SetEphemeralUDPPortRange(10000, 60000)
		if *verbose {
			log.Printf("Configured NAT 1:1 mapping to %s with UDP ports 10000-60000", *nat1to1IP)
		}
	}

	// Create API with media engine, interceptors, and settings
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(i),
		webrtc.WithSettingEngine(se),
	)

	// Create PeerConnection configuration with ICE servers
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}

	// Add TURN server if configured
	if *turnServer != "" {
		turnICE := webrtc.ICEServer{
			URLs: []string{*turnServer},
		}
		if *turnUser != "" {
			turnICE.Username = *turnUser
			turnICE.Credential = *turnPass
			turnICE.CredentialType = webrtc.ICECredentialTypePassword
		}
		iceServers = append(iceServers, turnICE)
		if *verbose {
			log.Printf("Added TURN server: %s", *turnServer)
		}
	}

	config := webrtc.Configuration{
		ICEServers: iceServers,
	}

	// Create PeerConnection
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		log.Fatalf("Failed to create peer connection: %v", err)
	}
	defer pc.Close()

	// Track DataChannel stats
	var dataChannelBytesRecv int64
	var dataChannelMsgCount int64
	var dataChannelOpen int32
	dataChannelDone := make(chan struct{})
	dataChannelStartTime := time.Now()

	// Create a dummy DataChannel to enable SCTP transport in the offer
	// This allows the receiver to create DataChannels that will be sent to us
	_, err = pc.CreateDataChannel("_sctp-init", nil)
	if err != nil {
		log.Fatalf("Failed to create dummy data channel: %v", err)
	}

	// Store last received bandwidth stats from receiver
	var lastRecvStats BandwidthStats
	var lastRecvStatsMu sync.Mutex

	// Latency tracking (note: one-way latency may be negative due to clock skew)
	var latencySum int64   // sum of all latencies in ms
	var latencyCount int64 // number of latency samples
	var latencyMin int64 = 999999999
	var latencyMax int64 = -999999999
	var lastSeq uint64      // for detecting packet loss
	var packetsLost int64   // count of lost packets
	var packetsReordered int64

	// Handle incoming DataChannel from receiver (side channel)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("OnDataChannel called: label='%s', id=%d", dc.Label(), dc.ID())

		dc.OnOpen(func() {
			log.Printf("DataChannel '%s' opened", dc.Label())
			atomic.StoreInt32(&dataChannelOpen, 1)
			dataChannelStartTime = time.Now()
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			recvTime := time.Now().UnixMilli()
			atomic.AddInt64(&dataChannelBytesRecv, int64(len(msg.Data)))
			newCount := atomic.AddInt64(&dataChannelMsgCount, 1)

			// Parse bandwidth stats from receiver
			var stats BandwidthStats
			if err := json.Unmarshal(msg.Data, &stats); err == nil {
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
		})

		dc.OnClose(func() {
			if *verbose {
				log.Println("DataChannel closed")
			}
			close(dataChannelDone)
		})
	})

	// Create video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		"video",
		"webrtc-send",
	)
	if err != nil {
		log.Fatalf("Failed to create video track: %v", err)
	}

	// Add track to peer connection
	sender, err := pc.AddTrack(videoTrack)
	if err != nil {
		log.Fatalf("Failed to add track: %v", err)
	}

	// Handle RTCP packets (PLI, NACK, etc.)
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(rtcpBuf)
			if err != nil {
				if err == io.EOF {
					return
				}
				log.Printf("RTCP read error: %v", err)
				continue
			}
			if *verbose && n > 0 {
				log.Printf("Received RTCP packet (%d bytes)", n)
			}
		}
	}()

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})

	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		close(done)
	}()

	// Connection state handler
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if *verbose {
			log.Printf("Connection state: %s", state)
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			close(done)
		}
	})

	// ICE connection state handler
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if *verbose {
			log.Printf("ICE connection state: %s", state)
		}
	})

	// ICE candidate handler
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil && *verbose {
			log.Printf("ICE candidate: %s", c.String())
		}
	})

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Fatalf("Failed to create offer: %v", err)
	}

	// Start ICE gathering
	gatherComplete := webrtc.GatheringCompletePromise(pc)

	// Set local description
	if err := pc.SetLocalDescription(offer); err != nil {
		log.Fatalf("Failed to set local description: %v", err)
	}

	// Wait for ICE gathering
	<-gatherComplete

	if *verbose {
		log.Printf("ICE gathering complete, sending offer to WHIP endpoint")
	}

	// Send offer to WHIP endpoint
	resp, err := http.Post(*whipURL, "application/sdp", bytes.NewReader([]byte(pc.LocalDescription().SDP)))
	if err != nil {
		log.Fatalf("Failed to send WHIP offer: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Fatalf("WHIP endpoint returned error %d: %s", resp.StatusCode, string(body))
	}

	// Read answer
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read WHIP answer: %v", err)
	}

	if *verbose {
		log.Printf("Received WHIP answer (%d bytes)", len(answer))
	}

	// Set remote description
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(answer),
	}); err != nil {
		log.Fatalf("Failed to set remote description: %v", err)
	}

	// Wait for connection to be established
	connectedCh := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			close(connectedCh)
		}
	})

	select {
	case <-connectedCh:
		if *verbose {
			log.Println("WebRTC connection established")
		}
	case <-time.After(30 * time.Second):
		log.Fatal("Timeout waiting for connection")
	case <-done:
		return
	}

	// Sender stats (use atomics for goroutine access)
	var senderNALCount int64
	var senderTotalBytes int64
	senderStartTime := time.Now()

	// Start periodic JSON output (every 1s to stdout)
	statsStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-statsStop:
				return
			case <-ticker.C:
				if atomic.LoadInt32(&dataChannelOpen) != 1 {
					continue
				}

				// Get sender stats
				curBytes := atomic.LoadInt64(&senderTotalBytes)
				senderElapsed := time.Since(senderStartTime)
				var senderMbps float64
				if senderElapsed.Seconds() > 0 {
					senderMbps = float64(curBytes*8) / senderElapsed.Seconds() / 1e6
				}

				// Get receiver stats
				lastRecvStatsMu.Lock()
				recvStats := lastRecvStats
				latCount := latencyCount
				latSum := latencySum
				latMin := latencyMin
				latMax := latencyMax
				pktLost := packetsLost
				pktReorder := packetsReordered
				lastRecvStatsMu.Unlock()

				var avgLat float64
				if latCount > 0 {
					avgLat = float64(latSum) / float64(latCount)
				}

				// Calculate datachannel bitrate
				dcBytes := atomic.LoadInt64(&dataChannelBytesRecv)
				dcElapsed := time.Since(dataChannelStartTime).Seconds()
				var dcMbps float64
				if dcElapsed > 0 {
					dcMbps = float64(dcBytes*8) / dcElapsed / 1e6
				}

				// Output periodic JSON summary
				summary := Summary{
					SenderBytes:      curBytes,
					SenderBitrate:    senderMbps,
					SenderElapsedMs:  senderElapsed.Milliseconds(),
					ReceiverBytes:    recvStats.BytesRecv,
					ReceiverBitrate:  recvStats.BitrateMbps,
					DataChannelMbps:  dcMbps,
					LatencyAvg:       avgLat,
					LatencyMin:       latMin,
					LatencyMax:       latMax,
					LatencySamples:   latCount,
					PacketsLost:      pktLost,
					PacketsReordered: pktReorder,
				}
				json.NewEncoder(os.Stdout).Encode(summary)

				// Also log to stderr if verbose
				if *verbose {
					currentCount := atomic.LoadInt64(&dataChannelMsgCount)
					log.Printf("DataChannel: %d msgs, %d bytes, avg=%.3f Mbps",
						currentCount, dcBytes, dcMbps)
				}
			}
		}
	}()

	// Read H264 NAL units from stdin and send them
	reader := h264.NewAnnexBReader(os.Stdin)
	packetizer := h264.NewRTPPacketizer(0x12345678, 1200)

	// Frame duration in ms
	frameDurationMs := uint32(1000 / *framerate)

	for {
		select {
		case <-done:
			goto finish
		default:
		}

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

		// Packetize NAL into RTP packets
		packets, isKeyframe := packetizer.Packetize(nal)

		// Send each RTP packet
		for _, pkt := range packets {
			if err := videoTrack.WriteRTP(pkt); err != nil {
				log.Printf("Error writing RTP: %v", err)
				break
			}
		}

		atomic.AddInt64(&senderNALCount, 1)
		atomic.AddInt64(&senderTotalBytes, int64(len(nal.Data)))

		nalCount := atomic.LoadInt64(&senderNALCount)
		if *verbose && nalCount%100 == 0 {
			totalBytes := atomic.LoadInt64(&senderTotalBytes)
			elapsed := time.Since(senderStartTime).Seconds()
			bitrate := float64(totalBytes*8) / elapsed / 1e6
			log.Printf("Sent %d NALs, %.2f MB, %.2f Mbps", nalCount, float64(totalBytes)/1e6, bitrate)
		}

		// Advance timestamp for next frame (approximate - assumes each NAL is one frame)
		// NAL types 1 (non-IDR slice) and 5 (IDR slice) are video frames
		if isKeyframe || nal.Type == 1 || nal.Type == 5 {
			packetizer.NextTimestamp(frameDurationMs)
		}
	}

finish:
	nalCount := atomic.LoadInt64(&senderNALCount)
	totalBytes := atomic.LoadInt64(&senderTotalBytes)
	elapsed := time.Since(senderStartTime)
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
	case <-dataChannelDone:
		if *verbose {
			log.Println("Data channel closed")
		}
	case <-time.After(*dataChannelTimeout):
		if *verbose {
			log.Println("Data channel timeout")
		}
	}

	// Stop periodic JSON output
	close(statsStop)

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

	// Calculate final datachannel bitrate
	finalDcBytes := atomic.LoadInt64(&dataChannelBytesRecv)
	finalDcElapsed := time.Since(dataChannelStartTime).Seconds()
	var finalDcMbps float64
	if finalDcElapsed > 0 {
		finalDcMbps = float64(finalDcBytes*8) / finalDcElapsed / 1e6
	}

	summary := Summary{
		SenderBytes:      totalBytes,
		SenderBitrate:    senderBitrate,
		SenderElapsedMs:  elapsed.Milliseconds(),
		ReceiverBytes:    finalStats.BytesRecv,
		ReceiverBitrate:  finalStats.BitrateMbps,
		DataChannelMbps:  finalDcMbps,
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
	dcCount := atomic.LoadInt64(&dataChannelMsgCount)
	if dcCount > 0 {
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
