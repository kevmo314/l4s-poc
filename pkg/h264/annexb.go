package h264

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
)

// NAL unit types
const (
	NALTypeSlice    = 1
	NALTypeIDR      = 5
	NALTypeSEI      = 6
	NALTypeSPS      = 7
	NALTypePPS      = 8
	NALTypeAUD      = 9
	NALTypeFiller   = 12
)

// StartCode is the Annex B NAL unit delimiter
var StartCode = []byte{0x00, 0x00, 0x00, 0x01}
var StartCode3 = []byte{0x00, 0x00, 0x01}

// NALUnit represents a single H264 NAL unit
type NALUnit struct {
	Type    uint8
	Data    []byte // Includes NAL header byte
}

// NALType returns the NAL unit type from the first byte
func NALType(b byte) uint8 {
	return b & 0x1F
}

// IsKeyframe returns true if this NAL unit is part of a keyframe
func (n *NALUnit) IsKeyframe() bool {
	return n.Type == NALTypeSPS || n.Type == NALTypePPS || n.Type == NALTypeIDR
}

// AnnexBReader reads NAL units from an Annex B byte stream
type AnnexBReader struct {
	r      *bufio.Reader
	buf    []byte
	hasEOF bool
}

// NewAnnexBReader creates a new Annex B reader
func NewAnnexBReader(r io.Reader) *AnnexBReader {
	return &AnnexBReader{
		r:   bufio.NewReaderSize(r, 1024*1024), // 1MB buffer for large NALs
		buf: make([]byte, 0, 64*1024),          // 64KB initial capacity
	}
}

// ReadNAL reads the next NAL unit from the stream
// Returns the NAL unit data WITHOUT the start code prefix
func (r *AnnexBReader) ReadNAL() (*NALUnit, error) {
	if r.hasEOF && len(r.buf) == 0 {
		return nil, io.EOF
	}

	// Skip any leading start code from previous read
	if bytes.HasPrefix(r.buf, StartCode) {
		r.buf = r.buf[4:]
	} else if bytes.HasPrefix(r.buf, StartCode3) {
		r.buf = r.buf[3:]
	}

	// Find the next NAL unit
	for {
		if r.hasEOF {
			break
		}

		b, err := r.r.ReadByte()
		if err == io.EOF {
			r.hasEOF = true
			break
		}
		if err != nil {
			return nil, err
		}

		r.buf = append(r.buf, b)

		// Check for start code at the end of buffer
		if len(r.buf) >= 4 && bytes.Equal(r.buf[len(r.buf)-4:], StartCode) {
			// Found 4-byte start code
			if len(r.buf) > 4 {
				// Return everything before this start code as a NAL unit
				nalData := make([]byte, len(r.buf)-4)
				copy(nalData, r.buf[:len(r.buf)-4])
				// Keep just the start code for next iteration (will be stripped at start)
				r.buf = append(r.buf[:0], StartCode...)

				if len(nalData) > 0 {
					nal := &NALUnit{
						Type: NALType(nalData[0]),
						Data: nalData,
					}
					return nal, nil
				}
			}
			// If we only have the start code, clear and continue
			r.buf = r.buf[:0]
			continue
		}

		if len(r.buf) >= 3 && bytes.Equal(r.buf[len(r.buf)-3:], StartCode3) {
			// Found 3-byte start code
			if len(r.buf) > 3 {
				nalData := make([]byte, len(r.buf)-3)
				copy(nalData, r.buf[:len(r.buf)-3])
				r.buf = append(r.buf[:0], StartCode3...)

				if len(nalData) > 0 {
					nal := &NALUnit{
						Type: NALType(nalData[0]),
						Data: nalData,
					}
					return nal, nil
				}
			}
			r.buf = r.buf[:0]
			continue
		}
	}

	// EOF reached, return remaining buffer as last NAL
	if len(r.buf) > 0 {
		nalData := r.buf
		if len(nalData) > 0 {
			nal := &NALUnit{
				Type: NALType(nalData[0]),
				Data: nalData,
			}
			r.buf = r.buf[:0]
			return nal, nil
		}
	}

	return nil, io.EOF
}

// AnnexBWriter writes NAL units in Annex B format
type AnnexBWriter struct {
	w io.Writer
}

// NewAnnexBWriter creates a new Annex B writer
func NewAnnexBWriter(w io.Writer) *AnnexBWriter {
	return &AnnexBWriter{w: w}
}

// WriteNAL writes a NAL unit with start code prefix
func (w *AnnexBWriter) WriteNAL(nal *NALUnit) error {
	// Write 4-byte start code
	if _, err := w.w.Write(StartCode); err != nil {
		return err
	}
	// Write NAL data
	_, err := w.w.Write(nal.Data)
	return err
}

// WriteNALData writes raw NAL data with start code prefix
func (w *AnnexBWriter) WriteNALData(data []byte) error {
	if _, err := w.w.Write(StartCode); err != nil {
		return err
	}
	_, err := w.w.Write(data)
	return err
}

// LengthPrefixedReader reads length-prefixed NAL units (for QUIC)
type LengthPrefixedReader struct {
	r io.Reader
}

// NewLengthPrefixedReader creates a reader for length-prefixed NAL format
func NewLengthPrefixedReader(r io.Reader) *LengthPrefixedReader {
	return &LengthPrefixedReader{r: r}
}

// ReadNAL reads a length-prefixed NAL unit
func (r *LengthPrefixedReader) ReadNAL() (*NALUnit, error) {
	var length uint32
	if err := binary.Read(r.r, binary.BigEndian, &length); err != nil {
		return nil, err
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, io.ErrUnexpectedEOF
	}

	return &NALUnit{
		Type: NALType(data[0]),
		Data: data,
	}, nil
}

// LengthPrefixedWriter writes length-prefixed NAL units (for QUIC)
type LengthPrefixedWriter struct {
	w io.Writer
}

// NewLengthPrefixedWriter creates a writer for length-prefixed NAL format
func NewLengthPrefixedWriter(w io.Writer) *LengthPrefixedWriter {
	return &LengthPrefixedWriter{w: w}
}

// WriteNAL writes a length-prefixed NAL unit
func (w *LengthPrefixedWriter) WriteNAL(nal *NALUnit) error {
	if err := binary.Write(w.w, binary.BigEndian, uint32(len(nal.Data))); err != nil {
		return err
	}
	_, err := w.w.Write(nal.Data)
	return err
}

// WriteNALData writes raw NAL data with length prefix
func (w *LengthPrefixedWriter) WriteNALData(data []byte) error {
	if err := binary.Write(w.w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err := w.w.Write(data)
	return err
}
