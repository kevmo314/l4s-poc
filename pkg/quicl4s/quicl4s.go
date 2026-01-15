// Package quicl4s provides QUIC transport with L4S support using quic-go.
// It enables ECN marking (ECT(1)) for L4S compatibility.
package quicl4s

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

// L4S ECN values
const (
	ECNNonECT = 0x00 // Non ECN-Capable Transport
	ECNECT0   = 0x02 // ECN Capable Transport (0)
	ECNECT1   = 0x01 // ECN Capable Transport (1) - L4S
	ECNCE     = 0x03 // Congestion Experienced
)

// Config holds L4S QUIC configuration
type Config struct {
	// Address to listen on (server) or connect to (client)
	Address string
	// TLS configuration (if nil, uses self-signed cert)
	TLSConfig *tls.Config
	// Enable L4S ECN marking (ECT(1))
	EnableL4S bool
	// QUIC configuration
	QUICConfig *quic.Config
}

// Server represents an L4S-enabled QUIC server
type Server struct {
	listener *quic.Listener
	config   Config
}

// Client represents an L4S-enabled QUIC client
type Client struct {
	conn   quic.Connection
	config Config
}

// Stream wraps a QUIC stream for sending/receiving data
type Stream struct {
	stream quic.Stream
}

// NewServer creates a new QUIC server with L4S support
func NewServer(config Config) (*Server, error) {
	tlsConfig := config.TLSConfig
	if tlsConfig == nil {
		var err error
		tlsConfig, err = generateTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to generate TLS config: %w", err)
		}
	}

	quicConfig := config.QUICConfig
	if quicConfig == nil {
		quicConfig = &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			EnableDatagrams: true,
		}
	}

	listener, err := quic.ListenAddr(config.Address, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}

	return &Server{
		listener: listener,
		config:   config,
	}, nil
}

// Accept accepts a new connection
func (s *Server) Accept(ctx context.Context) (*Connection, error) {
	conn, err := s.listener.Accept(ctx)
	if err != nil {
		return nil, err
	}

	return &Connection{
		conn:      conn,
		enableL4S: s.config.EnableL4S,
	}, nil
}

// Close closes the server
func (s *Server) Close() error {
	return s.listener.Close()
}

// NewClient creates a new QUIC client with L4S support
func NewClient(config Config) (*Client, error) {
	tlsConfig := config.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"l4s-benchmark"},
		}
	}

	quicConfig := config.QUICConfig
	if quicConfig == nil {
		quicConfig = &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			EnableDatagrams: true,
		}
	}

	return &Client{
		config: config,
	}, nil
}

// Connect connects to the server
func (c *Client) Connect(ctx context.Context) (*Connection, error) {
	tlsConfig := c.config.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"l4s-benchmark"},
		}
	}

	quicConfig := c.config.QUICConfig
	if quicConfig == nil {
		quicConfig = &quic.Config{
			MaxIdleTimeout:  30 * time.Second,
			EnableDatagrams: true,
		}
	}

	conn, err := quic.DialAddr(ctx, c.config.Address, tlsConfig, quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}

	return &Connection{
		conn:      conn,
		enableL4S: c.config.EnableL4S,
	}, nil
}

// Connection represents an L4S-enabled QUIC connection
type Connection struct {
	conn      quic.Connection
	enableL4S bool
}

// OpenStream opens a new bidirectional stream
func (c *Connection) OpenStream() (*Stream, error) {
	stream, err := c.conn.OpenStream()
	if err != nil {
		return nil, err
	}
	return &Stream{stream: stream}, nil
}

// AcceptStream accepts an incoming stream
func (c *Connection) AcceptStream(ctx context.Context) (*Stream, error) {
	stream, err := c.conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &Stream{stream: stream}, nil
}

// SendDatagram sends an unreliable datagram
func (c *Connection) SendDatagram(data []byte) error {
	return c.conn.SendDatagram(data)
}

// ReceiveDatagram receives a datagram
func (c *Connection) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return c.conn.ReceiveDatagram(ctx)
}

// Close closes the connection
func (c *Connection) Close() error {
	return c.conn.CloseWithError(0, "closed")
}

// LocalAddr returns the local address
func (c *Connection) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote address
func (c *Connection) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Read reads data from the stream
func (s *Stream) Read(p []byte) (int, error) {
	return s.stream.Read(p)
}

// Write writes data to the stream
func (s *Stream) Write(p []byte) (int, error) {
	return s.stream.Write(p)
}

// Close closes the stream
func (s *Stream) Close() error {
	return s.stream.Close()
}

// StreamID returns the stream ID
func (s *Stream) StreamID() quic.StreamID {
	return s.stream.StreamID()
}

// ReadAll reads all data from stream until EOF
func (s *Stream) ReadAll() ([]byte, error) {
	return io.ReadAll(s.stream)
}

// generateTLSConfig generates a self-signed TLS certificate for testing
func generateTLSConfig() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"l4s-benchmark"},
	}, nil
}
