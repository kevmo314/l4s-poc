#ifndef PICOQUIC_WRAPPER_H
#define PICOQUIC_WRAPPER_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque handles
typedef struct pq_context_t pq_context_t;
typedef struct pq_connection_t pq_connection_t;

// Callback for receiving data
typedef void (*pq_data_callback_t)(void* ctx, const uint8_t* data, size_t len);

// Create a QUIC context (server or client)
// cert_file and key_file only needed for server
pq_context_t* pq_create_context(int is_server, const char* cert_file, const char* key_file);

// Set congestion control algorithm ("prague", "cubic", "bbr", "newreno")
int pq_set_congestion_algorithm(pq_context_t* ctx, const char* algo_name);

// Server: start listening on port, returns 0 on success
int pq_listen(pq_context_t* ctx, uint16_t port);

// Client: connect to server, returns connection handle
pq_connection_t* pq_connect(pq_context_t* ctx, const char* server_name, uint16_t port);

// Server: accept a connection (blocking)
pq_connection_t* pq_accept(pq_context_t* ctx);

// Send data on stream 0 (reliable)
int pq_send_stream(pq_connection_t* conn, const uint8_t* data, size_t len);

// Finish stream 0 (send FIN, no more data)
int pq_finish_stream(pq_connection_t* conn);

// Wait for stream 0 to be fully sent and acknowledged (call after finish_stream)
// Returns 0 on success, -1 on error/timeout
int pq_wait_stream_complete(pq_connection_t* conn, int timeout_ms);

// Receive data from stream 0 (blocking, returns bytes received, -1 on error)
int pq_recv_stream(pq_connection_t* conn, uint8_t* buffer, size_t buffer_size);

// Send datagram (unreliable)
int pq_send_datagram(pq_connection_t* conn, const uint8_t* data, size_t len);

// Receive datagram (blocking, returns bytes received, -1 on error)
int pq_recv_datagram(pq_connection_t* conn, uint8_t* buffer, size_t buffer_size);

// Close connection
void pq_close_connection(pq_connection_t* conn);

// Destroy context
void pq_destroy_context(pq_context_t* ctx);

// Run the event loop for a short time (non-blocking, returns 0 on success)
int pq_process_events(pq_context_t* ctx, int timeout_ms);

// Check if connection is still open
int pq_is_connected(pq_connection_t* conn);

#ifdef __cplusplus
}
#endif

#endif // PICOQUIC_WRAPPER_H
