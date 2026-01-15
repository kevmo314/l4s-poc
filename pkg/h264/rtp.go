package h264

import (
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// DefaultMTU is the default Maximum Transmission Unit for RTP packets.
const DefaultMTU = 1200

// RTPPacketizer packetizes H264 NAL units into RTP packets.
type RTPPacketizer struct {
	payloader *codecs.H264Payloader
	mtu       uint16
	ssrc      uint32
	seqNum    uint16
	timestamp uint32
	clockRate uint32
}

// NewRTPPacketizer creates a new RTP packetizer for H264.
func NewRTPPacketizer(ssrc uint32, mtu uint16) *RTPPacketizer {
	if mtu == 0 {
		mtu = DefaultMTU
	}
	return &RTPPacketizer{
		payloader: &codecs.H264Payloader{
			DisableStapA: true, // Don't aggregate small NALs - keep 1:1 NAL to marker mapping
		},
		mtu:       mtu,
		ssrc:      ssrc,
		clockRate: 90000, // Standard H264 clock rate
	}
}

// SetSequenceNumber sets the starting sequence number.
func (p *RTPPacketizer) SetSequenceNumber(seqNum uint16) {
	p.seqNum = seqNum
}

// SetTimestamp sets the current timestamp.
func (p *RTPPacketizer) SetTimestamp(timestamp uint32) {
	p.timestamp = timestamp
}

// NextTimestamp advances the timestamp by the given duration in time units at 90kHz.
func (p *RTPPacketizer) NextTimestamp(frameDurationMs uint32) {
	// 90000 Hz clock, so multiply by 90 to convert ms to timestamp units
	p.timestamp += frameDurationMs * 90
}

// Packetize packetizes a NAL unit into one or more RTP packets.
// Returns the RTP packets and whether the NAL unit contains a keyframe.
func (p *RTPPacketizer) Packetize(nal *NALUnit) ([]*rtp.Packet, bool) {
	// Use pion's H264Payloader to fragment the NAL
	payloads := p.payloader.Payload(p.mtu-12, nal.Data) // 12 bytes for RTP header

	isKeyframe := nal.IsKeyframe()
	packets := make([]*rtp.Packet, 0, len(payloads))

	for i, payload := range payloads {
		marker := i == len(payloads)-1 // Last packet of NAL gets marker bit

		packet := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         marker,
				PayloadType:    96, // Dynamic payload type for H264
				SequenceNumber: p.seqNum,
				Timestamp:      p.timestamp,
				SSRC:           p.ssrc,
			},
			Payload: payload,
		}

		packets = append(packets, packet)
		p.seqNum++
	}

	return packets, isKeyframe
}

// PacketizeRaw packetizes raw NAL data (without Annex B start code).
func (p *RTPPacketizer) PacketizeRaw(nalData []byte) []*rtp.Packet {
	payloads := p.payloader.Payload(p.mtu-12, nalData)
	packets := make([]*rtp.Packet, 0, len(payloads))

	for i, payload := range payloads {
		marker := i == len(payloads)-1

		packet := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         marker,
				PayloadType:    96,
				SequenceNumber: p.seqNum,
				Timestamp:      p.timestamp,
				SSRC:           p.ssrc,
			},
			Payload: payload,
		}

		packets = append(packets, packet)
		p.seqNum++
	}

	return packets
}

// RTPDepacketizer depacketizes RTP packets into H264 NAL units.
type RTPDepacketizer struct {
	packet codecs.H264Packet
}

// NewRTPDepacketizer creates a new RTP depacketizer for H264.
func NewRTPDepacketizer() *RTPDepacketizer {
	return &RTPDepacketizer{}
}

// Depacketize depacketizes an RTP packet into H264 NAL data.
// Returns the NAL data and whether this is a complete NAL unit (marker bit set).
func (d *RTPDepacketizer) Depacketize(packet *rtp.Packet) ([]byte, bool, error) {
	nalData, err := d.packet.Unmarshal(packet.Payload)
	if err != nil {
		return nil, false, err
	}
	return nalData, packet.Marker, nil
}

// IsPartitionHead returns true if this packet is the start of a NAL unit.
func (d *RTPDepacketizer) IsPartitionHead(packet *rtp.Packet) bool {
	checker := &codecs.H264PartitionHeadChecker{}
	return checker.IsPartitionHead(packet.Payload)
}
