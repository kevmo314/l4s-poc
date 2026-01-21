// webrtc-send reads H264 NAL units from stdin and sends them via WebRTC.
// It also receives data from the receiver via DataChannel and writes it to stdout (for benchmarking side channel).
// Usage: webrtc-send --whip-url http://receiver:8080/whip
package main

import (
	"bytes"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kevmo314/l4s/pkg/h264"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

func main() {
	whipURL := flag.String("whip-url", "http://127.0.0.1:8080/whip", "WHIP endpoint URL")
	verbose := flag.Bool("v", false, "Verbose output")
	framerate := flag.Int("fps", 30, "Frame rate for timestamp calculation")
	dataChannelTimeout := flag.Duration("dc-timeout", 5*time.Second, "Timeout to wait for data channel after sending video")
	nat1to1IP := flag.String("nat-1to1-ip", "", "Public IP for 1:1 NAT mapping (for Docker/NAT environments)")
	turnServer := flag.String("turn-server", "", "TURN server URL (e.g., turn:turn.example.com:3478)")
	turnUser := flag.String("turn-user", "", "TURN server username")
	turnPass := flag.String("turn-pass", "", "TURN server password")
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

	// Create interceptor registry
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		log.Fatalf("Failed to register interceptors: %v", err)
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

	// Handle incoming DataChannel from receiver (side channel)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("OnDataChannel called: label='%s', id=%d", dc.Label(), dc.ID())

		dc.OnOpen(func() {
			log.Printf("DataChannel '%s' opened", dc.Label())
			atomic.StoreInt32(&dataChannelOpen, 1)
			dataChannelStartTime = time.Now()
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			os.Stdout.Write(msg.Data)
			atomic.AddInt64(&dataChannelBytesRecv, int64(len(msg.Data)))
			atomic.AddInt64(&dataChannelMsgCount, 1)
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

	// Start datachannel stats logger
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
				if atomic.LoadInt32(&dataChannelOpen) == 1 {
					currentBytes := atomic.LoadInt64(&dataChannelBytesRecv)
					currentCount := atomic.LoadInt64(&dataChannelMsgCount)
					elapsed := time.Since(dataChannelStartTime).Seconds()
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
	packetizer := h264.NewRTPPacketizer(0x12345678, 1200)

	nalCount := 0
	totalBytes := int64(0)
	startTime := time.Now()

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

		nalCount++
		totalBytes += int64(len(nal.Data))

		if *verbose && nalCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
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
	close(statsStop)
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
	case <-dataChannelDone:
		if *verbose {
			log.Println("Data channel closed")
		}
	case <-time.After(*dataChannelTimeout):
		if *verbose {
			log.Println("Data channel timeout")
		}
	}

	// Print data channel stats
	dcBytes := atomic.LoadInt64(&dataChannelBytesRecv)
	dcCount := atomic.LoadInt64(&dataChannelMsgCount)
	if dcCount > 0 {
		log.Printf("Data channel: received %d messages, %d bytes", dcCount, dcBytes)
		dcElapsed := time.Since(dataChannelStartTime)
		if dcElapsed.Seconds() > 0 {
			dcBitrate := float64(dcBytes*8) / dcElapsed.Seconds() / 1e3
			log.Printf("Data channel bitrate: %.2f kbps", dcBitrate)
		}
	} else {
		log.Printf("Data channel: no messages received")
	}
}
