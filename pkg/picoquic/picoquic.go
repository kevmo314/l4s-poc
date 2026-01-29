// Package picoquic provides a Go wrapper for the picoquic QUIC library with L4S/Prague support.
package picoquic

/*
#include "picoquic_wrapper.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"io"
	"sync"
	"unsafe"
)

var (
	ErrConnectionClosed = errors.New("connection closed")
	ErrCreateContext    = errors.New("failed to create context")
	ErrCreateConnection = errors.New("failed to create connection")
	ErrListen           = errors.New("failed to listen")
	ErrSend             = errors.New("failed to send")
	ErrReceive          = errors.New("failed to receive")
)

// Context represents a QUIC context (either client or server).
type Context struct {
	ctx      *C.pq_context_t
	isServer bool
	mu       sync.Mutex
}

// Connection represents a QUIC connection.
type Connection struct {
	conn *C.pq_connection_t
	ctx  *Context
}

// NewServerContext creates a new server context with the given certificate and key files.
func NewServerContext(certFile, keyFile string) (*Context, error) {
	cCert := C.CString(certFile)
	cKey := C.CString(keyFile)
	defer C.free(unsafe.Pointer(cCert))
	defer C.free(unsafe.Pointer(cKey))

	ctx := C.pq_create_context(1, cCert, cKey)
	if ctx == nil {
		return nil, ErrCreateContext
	}

	return &Context{ctx: ctx, isServer: true}, nil
}

// NewClientContext creates a new client context.
func NewClientContext() (*Context, error) {
	ctx := C.pq_create_context(0, nil, nil)
	if ctx == nil {
		return nil, ErrCreateContext
	}

	return &Context{ctx: ctx, isServer: false}, nil
}

// SetCongestionAlgorithm sets the congestion control algorithm.
// Supported algorithms: "prague", "bbr", "cubic", "newreno"
func (c *Context) SetCongestionAlgorithm(algo string) error {
	cAlgo := C.CString(algo)
	defer C.free(unsafe.Pointer(cAlgo))

	if C.pq_set_congestion_algorithm(c.ctx, cAlgo) != 0 {
		return errors.New("failed to set congestion algorithm")
	}
	return nil
}

// Listen starts listening on the given port. Server only.
func (c *Context) Listen(port uint16) error {
	if !c.isServer {
		return errors.New("listen requires server context")
	}

	if C.pq_listen(c.ctx, C.uint16_t(port)) != 0 {
		return ErrListen
	}
	return nil
}

// Accept accepts a new connection. Server only, blocks until a connection arrives.
func (c *Context) Accept() (*Connection, error) {
	if !c.isServer {
		return nil, errors.New("accept requires server context")
	}

	conn := C.pq_accept(c.ctx)
	if conn == nil {
		return nil, ErrCreateConnection
	}

	return &Connection{conn: conn, ctx: c}, nil
}

// Connect connects to a server at the given address and port. Client only.
func (c *Context) Connect(serverName string, port uint16) (*Connection, error) {
	if c.isServer {
		return nil, errors.New("connect requires client context")
	}

	cServerName := C.CString(serverName)
	defer C.free(unsafe.Pointer(cServerName))

	conn := C.pq_connect(c.ctx, cServerName, C.uint16_t(port))
	if conn == nil {
		return nil, ErrCreateConnection
	}

	return &Connection{conn: conn, ctx: c}, nil
}

// Close destroys the context and all associated resources.
func (c *Context) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ctx != nil {
		C.pq_destroy_context(c.ctx)
		c.ctx = nil
	}
}

// Write sends data on stream 0.
func (c *Connection) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	n := C.pq_send_stream(c.conn, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
	if n < 0 {
		return 0, ErrSend
	}
	return int(n), nil
}

// FinishStream sends FIN on stream 0, indicating no more data will be sent.
func (c *Connection) FinishStream() error {
	if C.pq_finish_stream(c.conn) != 0 {
		return ErrSend
	}
	return nil
}

// WaitStreamComplete waits for all stream data to be acknowledged.
// Should be called after FinishStream to ensure all data is delivered.
func (c *Connection) WaitStreamComplete(timeoutMs int) error {
	if C.pq_wait_stream_complete(c.conn, C.int(timeoutMs)) != 0 {
		return errors.New("timeout waiting for stream to complete")
	}
	return nil
}

// Read reads data from stream 0.
func (c *Connection) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}

	n := C.pq_recv_stream(c.conn, (*C.uint8_t)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer)))
	if n < 0 {
		return 0, ErrReceive
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

// WriteDatagram sends a datagram.
func (c *Connection) WriteDatagram(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	n := C.pq_send_datagram(c.conn, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
	if n < 0 {
		return 0, ErrSend
	}
	return int(n), nil
}

// ReadDatagram reads a datagram.
func (c *Connection) ReadDatagram(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}

	n := C.pq_recv_datagram(c.conn, (*C.uint8_t)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer)))
	if n < 0 {
		return 0, ErrReceive
	}
	return int(n), nil
}

// IsConnected returns true if the connection is still open.
func (c *Connection) IsConnected() bool {
	return C.pq_is_connected(c.conn) != 0
}

// Close closes the connection.
func (c *Connection) Close() error {
	if c.conn != nil {
		C.pq_close_connection(c.conn)
	}
	return nil
}
