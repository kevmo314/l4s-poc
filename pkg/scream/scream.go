// Package scream provides a CGO wrapper for the SCReAM congestion control library.
package scream

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/scream/code -I${SRCDIR}/../../third_party/scream/code/wrapper_lib
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/scream/code/wrapper_lib -lscream -lstdc++ -lpthread

#include <stdlib.h>
#include <stdint.h>

// Function declarations from screamtx_plugin_wrapper.cpp
typedef void (*ScreamSenderPushCallBack)(uint8_t *, uint8_t *, uint8_t);

void ScreamSenderPluginInit(uint32_t ssrc, const char *arg_string, uint8_t *cb_data_arg, ScreamSenderPushCallBack callback);
void ScreamSenderPluginUpdate(uint32_t ssrc, const char *arg_string);
void ScreamSenderGlobalPluginInit(uint32_t ssrc, const char *arg_string, uint8_t *cb_data_arg, ScreamSenderPushCallBack callback);
void ScreamSenderPush(uint8_t *buf_rtp, uint32_t recvlen, uint16_t seq, uint8_t payload_type, uint32_t timestamp, uint32_t ssrc, uint8_t marker);
uint8_t ScreamSenderRtcpPush(uint8_t *buf_rtcp, uint32_t recvlen);
void ScreamSenderGetTargetRate(uint32_t ssrc, uint32_t *rate_p, uint32_t *force_idr_p);
void ScreamSenderStats(char *s, uint32_t *len, uint32_t ssrc, uint8_t clear);
void ScreamSenderStatsHeader(char *s, uint32_t *len);

// Callback implementation
extern void goScreamCallback(uint8_t *cbData, uint8_t *rtpBuf, uint8_t status);

static void scream_callback_wrapper(uint8_t *cbData, uint8_t *rtpBuf, uint8_t status) {
	goScreamCallback(cbData, rtpBuf, status);
}

static void scream_init_wrapper(uint32_t ssrc, const char *args, uint8_t *cb_data) {
	ScreamSenderPluginInit(ssrc, args, cb_data, scream_callback_wrapper);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// Sender implements SCReAM congestion control for sending media.
type Sender struct {
	ssrc      uint32
	mu        sync.Mutex
	onPacket  func(rtpData []byte)
	cbDataPtr *C.uint8_t // Pointer used as callback context
}

var (
	senderRegistry   = make(map[uintptr]*Sender)
	senderRegistryMu sync.RWMutex
)

//export goScreamCallback
func goScreamCallback(cbData *C.uint8_t, rtpBuf *C.uint8_t, status C.uint8_t) {
	// Find the sender by callback data pointer
	senderRegistryMu.RLock()
	sender, ok := senderRegistry[uintptr(unsafe.Pointer(cbData))]
	senderRegistryMu.RUnlock()

	if !ok || sender.onPacket == nil {
		return
	}

	// Copy RTP data to Go slice
	// Note: We need to determine the RTP packet size from the header
	// For now, status == 0 means packet was dropped due to full queue
	if status == 0 {
		return
	}

	// TODO: Implement proper RTP packet extraction
	// The callback receives the RTP buffer that should be sent
}

// NewSender creates a new SCReAM sender for the given SSRC.
// args is a command-line style argument string for SCReAM configuration.
// Example: "-minrate 1000 -maxrate 50000"
func NewSender(ssrc uint32, args string, onPacket func(rtpData []byte)) (*Sender, error) {
	s := &Sender{
		ssrc:     ssrc,
		onPacket: onPacket,
	}

	// Allocate callback data pointer (use as unique identifier)
	s.cbDataPtr = (*C.uint8_t)(C.malloc(1))

	// Register sender
	senderRegistryMu.Lock()
	senderRegistry[uintptr(unsafe.Pointer(s.cbDataPtr))] = s
	senderRegistryMu.Unlock()

	// Convert args to C string
	cArgs := C.CString(args)
	defer C.free(unsafe.Pointer(cArgs))

	// Initialize SCReAM
	C.scream_init_wrapper(C.uint32_t(ssrc), cArgs, s.cbDataPtr)

	return s, nil
}

// Push pushes an RTP packet to the SCReAM queue.
func (s *Sender) Push(rtpData []byte, seq uint16, payloadType uint8, timestamp uint32, marker bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	markerByte := C.uint8_t(0)
	if marker {
		markerByte = 1
	}

	C.ScreamSenderPush(
		(*C.uint8_t)(unsafe.Pointer(&rtpData[0])),
		C.uint32_t(len(rtpData)),
		C.uint16_t(seq),
		C.uint8_t(payloadType),
		C.uint32_t(timestamp),
		C.uint32_t(s.ssrc),
		markerByte,
	)
}

// ProcessRTCPFeedback processes an RTCP feedback packet (e.g., from receiver).
func (s *Sender) ProcessRTCPFeedback(rtcpData []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := C.ScreamSenderRtcpPush(
		(*C.uint8_t)(unsafe.Pointer(&rtcpData[0])),
		C.uint32_t(len(rtcpData)),
	)
	return result != 0
}

// GetTargetRate returns the current target bitrate and whether to force an IDR frame.
func (s *Sender) GetTargetRate() (rateBps uint32, forceIDR bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cRate, cForceIDR C.uint32_t
	C.ScreamSenderGetTargetRate(C.uint32_t(s.ssrc), &cRate, &cForceIDR)
	return uint32(cRate), cForceIDR != 0
}

// UpdateBitrateLimits updates the min/max bitrate limits.
func (s *Sender) UpdateBitrateLimits(minRateKbps, maxRateKbps uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	args := fmt.Sprintf("-minrate %d -maxrate %d", minRateKbps, maxRateKbps)
	cArgs := C.CString(args)
	defer C.free(unsafe.Pointer(cArgs))

	C.ScreamSenderPluginUpdate(C.uint32_t(s.ssrc), cArgs)
}

// Stats returns the current SCReAM statistics.
func (s *Sender) Stats(clear bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	statsBuf := make([]byte, 2048)
	var statsLen C.uint32_t

	clearByte := C.uint8_t(0)
	if clear {
		clearByte = 1
	}

	C.ScreamSenderStats(
		(*C.char)(unsafe.Pointer(&statsBuf[0])),
		&statsLen,
		C.uint32_t(s.ssrc),
		clearByte,
	)

	return string(statsBuf[:statsLen])
}

// StatsHeader returns the stats CSV header.
func StatsHeader() string {
	headerBuf := make([]byte, 1024)
	var headerLen C.uint32_t

	C.ScreamSenderStatsHeader(
		(*C.char)(unsafe.Pointer(&headerBuf[0])),
		&headerLen,
	)

	return string(headerBuf[:headerLen])
}

// Close releases resources associated with the sender.
func (s *Sender) Close() error {
	senderRegistryMu.Lock()
	delete(senderRegistry, uintptr(unsafe.Pointer(s.cbDataPtr)))
	senderRegistryMu.Unlock()

	if s.cbDataPtr != nil {
		C.free(unsafe.Pointer(s.cbDataPtr))
		s.cbDataPtr = nil
	}
	return nil
}
