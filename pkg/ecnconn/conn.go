// Package ecnconn provides an ECN-aware UDP connection wrapper for L4S support.
package ecnconn

import (
	"net"
	"sync"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// ECN values for IP header TOS field (lower 2 bits)
const (
	ECNNotECT = 0x00 // Not ECN-Capable Transport
	ECNEct1   = 0x01 // ECN-Capable Transport(1) - Used by L4S
	ECNEct0   = 0x02 // ECN-Capable Transport(0) - Classic ECN
	ECNCE     = 0x03 // Congestion Experienced
)

// Conn wraps a net.UDPConn with ECN support for both reading and writing.
type Conn struct {
	conn   *net.UDPConn
	conn4  *ipv4.PacketConn
	conn6  *ipv6.PacketConn
	isIPv6 bool
	fd     int

	// Default ECN value for outgoing packets
	defaultECN uint8

	mu sync.Mutex
}

// WrapConn wraps an existing UDP connection with ECN support.
func WrapConn(conn *net.UDPConn) (*Conn, error) {
	ec := &Conn{
		conn:       conn,
		defaultECN: ECNEct1, // Default to ECT(1) for L4S
	}

	// Get the file descriptor
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}

	var sockErr error
	err = rawConn.Control(func(fd uintptr) {
		ec.fd = int(fd)

		// Enable receiving TOS/Traffic Class
		// IP_RECVTOS for IPv4
		sockErr = syscall.SetsockoptInt(ec.fd, syscall.IPPROTO_IP, syscall.IP_RECVTOS, 1)
		if sockErr != nil {
			return
		}
		// IPV6_RECVTCLASS for IPv6 (set even for v4 in case of dual-stack)
		_ = syscall.SetsockoptInt(ec.fd, syscall.IPPROTO_IPV6, syscall.IPV6_RECVTCLASS, 1)
	})
	if err != nil {
		return nil, err
	}
	if sockErr != nil {
		return nil, sockErr
	}

	// Determine if IPv6
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	ec.isIPv6 = localAddr.IP.To4() == nil

	if ec.isIPv6 {
		ec.conn6 = ipv6.NewPacketConn(conn)
	} else {
		ec.conn4 = ipv4.NewPacketConn(conn)
	}

	// Set default TOS to ECT(1) for L4S
	if err := ec.SetTOS(ECNEct1); err != nil {
		// Non-fatal, continue anyway
	}

	return ec, nil
}

// ListenUDP creates a new ECN-aware UDP listener.
func ListenUDP(network string, laddr *net.UDPAddr) (*Conn, error) {
	conn, err := net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	return WrapConn(conn)
}

// DialUDP creates a new ECN-aware UDP connection.
func DialUDP(network string, laddr, raddr *net.UDPAddr) (*Conn, error) {
	conn, err := net.DialUDP(network, laddr, raddr)
	if err != nil {
		return nil, err
	}
	return WrapConn(conn)
}

// SetTOS sets the TOS/DSCP value for outgoing packets (includes ECN bits).
func (c *Conn) SetTOS(tos int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isIPv6 {
		return c.conn6.SetTrafficClass(tos)
	}
	return c.conn4.SetTOS(tos)
}

// SetDefaultECN sets the default ECN value for outgoing packets.
func (c *Conn) SetDefaultECN(ecn uint8) error {
	c.mu.Lock()
	c.defaultECN = ecn & 0x03
	c.mu.Unlock()
	return c.SetTOS(int(ecn & 0x03))
}

// ReadMsgUDP reads a message from the connection, returning the ECN value.
func (c *Conn) ReadMsgUDP(b []byte) (n int, ecn uint8, addr *net.UDPAddr, err error) {
	// Use recvmsg to get ancillary data containing TOS
	oob := make([]byte, 128) // Buffer for control messages
	var oobn, flags int

	n, oobn, flags, addr, err = c.conn.ReadMsgUDP(b, oob)
	if err != nil {
		return 0, 0, nil, err
	}
	_ = flags // unused

	// Parse the control messages to extract TOS/ECN
	ecn = parseECNFromOOB(oob[:oobn])
	return n, ecn, addr, nil
}

// parseECNFromOOB extracts ECN bits from the out-of-band control message data.
func parseECNFromOOB(oob []byte) uint8 {
	// Parse socket control messages
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0
	}

	for _, cmsg := range cmsgs {
		if cmsg.Header.Level == syscall.IPPROTO_IP && cmsg.Header.Type == syscall.IP_TOS {
			if len(cmsg.Data) >= 1 {
				return cmsg.Data[0] & 0x03 // ECN is bottom 2 bits
			}
		}
		if cmsg.Header.Level == syscall.IPPROTO_IPV6 && cmsg.Header.Type == syscall.IPV6_TCLASS {
			if len(cmsg.Data) >= 4 {
				// Traffic class is a 32-bit integer
				return uint8(cmsg.Data[0]) & 0x03
			}
		}
	}
	return 0
}

// WriteMsgUDP writes a message with the specified ECN value.
// Note: Per-packet ECN requires modifying TOS via socket option before each send.
func (c *Conn) WriteMsgUDP(b []byte, ecn uint8, addr *net.UDPAddr) (n int, err error) {
	// Set TOS before sending (this is socket-level, not per-packet on most systems)
	if err := c.SetTOS(int(ecn & 0x03)); err != nil {
		return 0, err
	}
	return c.conn.WriteToUDP(b, addr)
}

// WriteToUDP writes a message using the default ECN value.
func (c *Conn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	c.mu.Lock()
	ecn := c.defaultECN
	c.mu.Unlock()
	return c.WriteMsgUDP(b, ecn, addr)
}

// ReadFromUDP reads a message, discarding ECN information.
// Use ReadMsgUDP if you need ECN values.
func (c *Conn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, _, addr, err := c.ReadMsgUDP(b)
	return n, addr, err
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Close closes the connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// UDPConn returns the underlying net.UDPConn.
func (c *Conn) UDPConn() *net.UDPConn {
	return c.conn
}

// SetReadBuffer sets the size of the operating system's receive buffer.
func (c *Conn) SetReadBuffer(bytes int) error {
	return c.conn.SetReadBuffer(bytes)
}

// SetWriteBuffer sets the size of the operating system's transmit buffer.
func (c *Conn) SetWriteBuffer(bytes int) error {
	return c.conn.SetWriteBuffer(bytes)
}

// ECNStats tracks ECN statistics for congestion feedback.
type ECNStats struct {
	mu        sync.Mutex
	ect0Count uint64
	ect1Count uint64
	ceCount   uint64
	notECT    uint64
}

// NewECNStats creates a new ECN statistics tracker.
func NewECNStats() *ECNStats {
	return &ECNStats{}
}

// Record records an ECN value.
func (s *ECNStats) Record(ecn uint8) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ecn & 0x03 {
	case ECNNotECT:
		s.notECT++
	case ECNEct1:
		s.ect1Count++
	case ECNEct0:
		s.ect0Count++
	case ECNCE:
		s.ceCount++
	}
}

// Get returns the current ECN statistics.
func (s *ECNStats) Get() (notECT, ect0, ect1, ce uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notECT, s.ect0Count, s.ect1Count, s.ceCount
}

// Reset resets the ECN statistics.
func (s *ECNStats) Reset() (notECT, ect0, ect1, ce uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notECT, ect0, ect1, ce = s.notECT, s.ect0Count, s.ect1Count, s.ceCount
	s.notECT, s.ect0Count, s.ect1Count, s.ceCount = 0, 0, 0, 0
	return
}
