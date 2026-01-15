// Package l4s provides L4S/ECN support for Pion WebRTC.
package l4s

import (
	"io"
	"sync"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Attribute keys for passing ECN information through the interceptor chain.
const (
	// AttrECN is the attribute key for ECN value (uint8).
	AttrECN = "ecn"
)

// ECN values for IP header TOS field (lower 2 bits)
const (
	ECNNotECT = 0x00 // Not ECN-Capable Transport
	ECNEct1   = 0x01 // ECN-Capable Transport(1) - Used by L4S
	ECNEct0   = 0x02 // ECN-Capable Transport(0) - Classic ECN
	ECNCE     = 0x03 // Congestion Experienced
)

// ECNTracker tracks ECN statistics for a stream.
type ECNTracker struct {
	mu            sync.Mutex
	ssrc          uint32
	ect0Count     uint64
	ect1Count     uint64
	ceCount       uint64
	notECTCount   uint64
	totalPackets  uint64
	lastSeqNum    uint16
	lastTimestamp uint32
}

// NewECNTracker creates a new ECN tracker for the given SSRC.
func NewECNTracker(ssrc uint32) *ECNTracker {
	return &ECNTracker{ssrc: ssrc}
}

// Record records an ECN value for a packet.
func (t *ECNTracker) Record(ecn uint8, seqNum uint16, timestamp uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.totalPackets++
	t.lastSeqNum = seqNum
	t.lastTimestamp = timestamp

	switch ecn & 0x03 {
	case ECNNotECT:
		t.notECTCount++
	case ECNEct1:
		t.ect1Count++
	case ECNEct0:
		t.ect0Count++
	case ECNCE:
		t.ceCount++
	}
}

// Stats returns the current ECN statistics.
func (t *ECNTracker) Stats() (ect0, ect1, ce, notECT, total uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ect0Count, t.ect1Count, t.ceCount, t.notECTCount, t.totalPackets
}

// CECount returns the number of CE-marked packets.
func (t *ECNTracker) CECount() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ceCount
}

// Reset resets the ECN statistics and returns the previous values.
func (t *ECNTracker) Reset() (ect0, ect1, ce, notECT uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ect0, ect1, ce, notECT = t.ect0Count, t.ect1Count, t.ceCount, t.notECTCount
	t.ect0Count, t.ect1Count, t.ceCount, t.notECTCount = 0, 0, 0, 0
	t.totalPackets = 0
	return
}

// Interceptor implements the L4S interceptor that tracks ECN marks.
type Interceptor struct {
	interceptor.NoOp

	mu       sync.RWMutex
	trackers map[uint32]*ECNTracker

	// ECN feedback generation callback (optional)
	onFeedback func(ssrc uint32, tracker *ECNTracker)
}

// InterceptorFactory creates L4S interceptors.
type InterceptorFactory struct {
	onFeedback func(ssrc uint32, tracker *ECNTracker)
}

// NewInterceptorFactory creates a new L4S interceptor factory.
func NewInterceptorFactory() *InterceptorFactory {
	return &InterceptorFactory{}
}

// SetFeedbackCallback sets the callback for ECN feedback generation.
func (f *InterceptorFactory) SetFeedbackCallback(cb func(ssrc uint32, tracker *ECNTracker)) {
	f.onFeedback = cb
}

// NewInterceptor creates a new L4S interceptor.
func (f *InterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return &Interceptor{
		trackers:   make(map[uint32]*ECNTracker),
		onFeedback: f.onFeedback,
	}, nil
}

// BindRemoteStream wraps the reader to extract ECN information from incoming packets.
func (i *Interceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	ssrc := info.SSRC

	i.mu.Lock()
	tracker := NewECNTracker(ssrc)
	i.trackers[ssrc] = tracker
	i.mu.Unlock()

	return interceptor.RTPReaderFunc(func(buf []byte, attrs interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attrs, err := reader.Read(buf, attrs)
		if err != nil {
			return n, attrs, err
		}

		// Extract ECN from attributes (set by ECN-aware UDP reader)
		if ecnVal, ok := attrs[AttrECN]; ok {
			if ecn, ok := ecnVal.(uint8); ok {
				// Parse RTP header to get sequence number and timestamp
				if n >= 12 {
					seqNum := uint16(buf[2])<<8 | uint16(buf[3])
					timestamp := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])

					tracker.Record(ecn, seqNum, timestamp)

					// Trigger feedback if CE was marked
					if ecn == ECNCE && i.onFeedback != nil {
						i.onFeedback(ssrc, tracker)
					}
				}
			}
		}

		return n, attrs, nil
	})
}

// UnbindRemoteStream removes the ECN tracker for the stream.
func (i *Interceptor) UnbindRemoteStream(info *interceptor.StreamInfo) {
	i.mu.Lock()
	delete(i.trackers, info.SSRC)
	i.mu.Unlock()
}

// GetTracker returns the ECN tracker for a given SSRC.
func (i *Interceptor) GetTracker(ssrc uint32) *ECNTracker {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.trackers[ssrc]
}

// GetAllTrackers returns all ECN trackers.
func (i *Interceptor) GetAllTrackers() map[uint32]*ECNTracker {
	i.mu.RLock()
	defer i.mu.RUnlock()

	result := make(map[uint32]*ECNTracker, len(i.trackers))
	for k, v := range i.trackers {
		result[k] = v
	}
	return result
}

// Close implements io.Closer.
func (i *Interceptor) Close() error {
	return nil
}

// ECNFeedbackWriter wraps an RTCP writer to inject ECN feedback.
type ECNFeedbackWriter struct {
	writer  interceptor.RTCPWriter
	tracker *ECNTracker
}

// Write writes RTCP packets, potentially injecting ECN feedback.
func (w *ECNFeedbackWriter) Write(pkts []rtcp.Packet, attrs interceptor.Attributes) (int, error) {
	// Pass through existing packets
	return w.writer.Write(pkts, attrs)
}

// LocalStreamWriter wraps an RTP writer for local stream handling.
type LocalStreamWriter struct {
	writer interceptor.RTPWriter
}

// Write writes RTP packets.
func (w *LocalStreamWriter) Write(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	return w.writer.Write(header, payload, attrs)
}

// Ensure interfaces are implemented
var (
	_ interceptor.Interceptor = (*Interceptor)(nil)
	_ interceptor.Factory     = (*InterceptorFactory)(nil)
	_ io.Closer               = (*Interceptor)(nil)
)
