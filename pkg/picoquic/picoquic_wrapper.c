#include "picoquic_wrapper.h"
#include "picoquic.h"
#include "picoquic_utils.h"
#include "picosocks.h"
#include "picoquic_packet_loop.h"
#include "picotls.h"

#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <pthread.h>
#include <unistd.h>
#include <netdb.h>
#include <arpa/inet.h>

// Simple ALPN selector that accepts any ALPN (returns first one)
static size_t simple_alpn_select(picoquic_quic_t* quic, ptls_iovec_t* list, size_t count) {
    (void)quic;
    if (count > 0) {
        fprintf(stderr, "DEBUG: ALPN selected: %.*s\n", (int)list[0].len, (char*)list[0].base);
        return 0;  // Select the first ALPN
    }
    return count;  // No ALPN to select
}

#define MAX_PACKET_SIZE 1500
#define RECV_BUFFER_SIZE (1024 * 1024)  // 1MB receive buffer
#define SEND_BUFFER_SIZE (1024 * 1024)  // 1MB send buffer

// Magic numbers to identify context vs connection
#define PQ_CONTEXT_MAGIC 0xC0FFEE01
#define PQ_CONNECTION_MAGIC 0xC0FFEE02

// Forward declarations
static int stream_callback(picoquic_cnx_t* cnx, uint64_t stream_id, uint8_t* bytes,
    size_t length, picoquic_call_back_event_t event, void* callback_ctx, void* stream_ctx);
static int loop_callback(picoquic_quic_t* quic, picoquic_packet_loop_cb_enum cb_mode,
    void* callback_ctx, void* callback_arg);

// Send queue item
typedef struct send_item_t {
    uint8_t* data;
    size_t len;
    int is_stream;  // 1 for stream, 0 for datagram
    int set_fin;    // Set FIN flag on stream
    struct send_item_t* next;
} send_item_t;

// Internal structures
struct pq_context_t {
    uint32_t magic;  // PQ_CONTEXT_MAGIC
    picoquic_quic_t* quic;
    int is_server;
    uint16_t listen_port;

    // Network thread
    picoquic_network_thread_ctx_t* thread_ctx;
    int thread_ret;

    // Pending connection for accept
    pq_connection_t* pending_conn;
    int has_pending_conn;
    pthread_mutex_t accept_lock;
    pthread_cond_t accept_cond;

    // For client connection
    pq_connection_t* client_conn;
};

struct pq_connection_t {
    uint32_t magic;  // PQ_CONNECTION_MAGIC
    pq_context_t* ctx;
    picoquic_cnx_t* cnx;

    // Stream 0 receive buffer (circular)
    uint8_t* recv_buffer;
    size_t recv_size;
    size_t recv_read;
    size_t recv_write;
    pthread_mutex_t recv_lock;
    pthread_cond_t recv_cond;

    // Send queue
    send_item_t* send_head;
    send_item_t* send_tail;
    pthread_mutex_t send_lock;

    // Datagram receive (single buffer, latest overwrites)
    uint8_t* dgram_buffer;
    size_t dgram_len;
    int dgram_ready;
    pthread_mutex_t dgram_lock;
    pthread_cond_t dgram_cond;

    // Connection state
    volatile int is_connected;
    volatile int is_closed;
    volatile int handshake_done;
    volatile int stream_fin_received;  // FIN received on stream 0
};

// Create a connection wrapper
static pq_connection_t* create_connection(pq_context_t* ctx, picoquic_cnx_t* cnx) {
    pq_connection_t* conn = calloc(1, sizeof(pq_connection_t));
    if (!conn) return NULL;

    conn->magic = PQ_CONNECTION_MAGIC;
    conn->ctx = ctx;
    conn->cnx = cnx;

    conn->recv_buffer = malloc(RECV_BUFFER_SIZE);
    conn->recv_size = RECV_BUFFER_SIZE;

    conn->dgram_buffer = malloc(MAX_PACKET_SIZE);

    if (!conn->recv_buffer || !conn->dgram_buffer) {
        free(conn->recv_buffer);
        free(conn->dgram_buffer);
        free(conn);
        return NULL;
    }

    pthread_mutex_init(&conn->recv_lock, NULL);
    pthread_cond_init(&conn->recv_cond, NULL);
    pthread_mutex_init(&conn->send_lock, NULL);
    pthread_mutex_init(&conn->dgram_lock, NULL);
    pthread_cond_init(&conn->dgram_cond, NULL);

    // Set this connection as the callback context
    picoquic_set_callback(cnx, stream_callback, conn);

    return conn;
}

// Queue data for sending (called from Go thread)
static int queue_send_ex(pq_connection_t* conn, const uint8_t* data, size_t len, int is_stream, int set_fin) {
    send_item_t* item = malloc(sizeof(send_item_t));
    if (!item) return -1;

    if (len > 0) {
        item->data = malloc(len);
        if (!item->data) {
            free(item);
            return -1;
        }
        memcpy(item->data, data, len);
    } else {
        item->data = NULL;
    }
    item->len = len;
    item->is_stream = is_stream;
    item->set_fin = set_fin;
    item->next = NULL;

    pthread_mutex_lock(&conn->send_lock);
    if (conn->send_tail) {
        conn->send_tail->next = item;
        conn->send_tail = item;
    } else {
        conn->send_head = conn->send_tail = item;
    }
    pthread_mutex_unlock(&conn->send_lock);

    return 0;
}

static int queue_send(pq_connection_t* conn, const uint8_t* data, size_t len, int is_stream) {
    return queue_send_ex(conn, data, len, is_stream, 0);
}

// Process send queue (called from network thread via wake_up callback)
static void process_send_queue(pq_connection_t* conn) {
    if (!conn || !conn->cnx) return;

    pthread_mutex_lock(&conn->send_lock);
    while (conn->send_head) {
        send_item_t* item = conn->send_head;
        conn->send_head = item->next;
        if (!conn->send_head) conn->send_tail = NULL;
        pthread_mutex_unlock(&conn->send_lock);

        if (item->is_stream) {
            if (item->set_fin) {
                fprintf(stderr, "DEBUG: Sending FIN on stream 0 (len=%zu)\n", item->len);
            }
            picoquic_add_to_stream(conn->cnx, 0, item->data, item->len, item->set_fin);
        } else {
            picoquic_queue_datagram_frame(conn->cnx, item->len, item->data);
        }

        if (item->data) free(item->data);
        free(item);

        pthread_mutex_lock(&conn->send_lock);
    }
    pthread_mutex_unlock(&conn->send_lock);
}

// Stream callback - handles incoming data and connection events
static int stream_callback(picoquic_cnx_t* cnx, uint64_t stream_id, uint8_t* bytes,
    size_t length, picoquic_call_back_event_t event, void* callback_ctx, void* stream_ctx) {

    if (!callback_ctx) return 0;

    // Check if this is a context or connection callback
    uint32_t* magic = (uint32_t*)callback_ctx;

    // Handle callbacks that happen before connection wrapper is set up
    if (*magic == PQ_CONTEXT_MAGIC) {
        pq_context_t* ctx = (pq_context_t*)callback_ctx;

        switch (event) {
            case picoquic_callback_request_alpn_list:
                // Add our ALPN to the list
                fprintf(stderr, "DEBUG: Adding ALPN 'l4s' to proposed list (from context)\n");
                if (bytes != NULL) {
                    picoquic_add_proposed_alpn((void*)bytes, "l4s");
                }
                return 0;
            case picoquic_callback_set_alpn:
                fprintf(stderr, "DEBUG: ALPN set (from context)\n");
                return 0;
            default:
                break;
        }

        // For server: if we get stream data or connection ready events, create wrapper now
        if (ctx->is_server && cnx != NULL) {
            picoquic_state_enum state = picoquic_get_cnx_state(cnx);
            if (state >= picoquic_state_server_almost_ready) {
                fprintf(stderr, "DEBUG: Creating connection wrapper on-demand (event=%d, state=%d)\n", event, state);
                pq_connection_t* new_conn = create_connection(ctx, cnx);
                if (new_conn) {
                    // Signal pending connection
                    pthread_mutex_lock(&ctx->accept_lock);
                    ctx->pending_conn = new_conn;
                    ctx->has_pending_conn = 1;
                    pthread_cond_signal(&ctx->accept_cond);
                    pthread_mutex_unlock(&ctx->accept_lock);

                    // Re-invoke callback with connection context
                    return stream_callback(cnx, stream_id, bytes, length, event, new_conn, stream_ctx);
                }
            }
        }
        return 0;
    }

    if (*magic != PQ_CONNECTION_MAGIC) {
        fprintf(stderr, "DEBUG: Unknown magic 0x%X\n", *magic);
        return 0;
    }

    pq_connection_t* conn = (pq_connection_t*)callback_ctx;

    switch (event) {
        case picoquic_callback_stream_data:
        case picoquic_callback_stream_fin:
            if (stream_id == 0 && length > 0) {
                pthread_mutex_lock(&conn->recv_lock);
                // Simple append (non-circular for now)
                size_t space = conn->recv_size - conn->recv_write;
                if (length <= space) {
                    memcpy(conn->recv_buffer + conn->recv_write, bytes, length);
                    conn->recv_write += length;
                }
                pthread_cond_signal(&conn->recv_cond);
                pthread_mutex_unlock(&conn->recv_lock);
            }
            if (event == picoquic_callback_stream_fin && stream_id == 0) {
                // Mark EOF on stream 0
                fprintf(stderr, "DEBUG: Received FIN on stream 0\n");
                pthread_mutex_lock(&conn->recv_lock);
                conn->stream_fin_received = 1;
                pthread_cond_signal(&conn->recv_cond);  // Wake up any waiting recv
                pthread_mutex_unlock(&conn->recv_lock);
            }
            break;

        case picoquic_callback_datagram:
            if (length > 0 && length <= MAX_PACKET_SIZE) {
                pthread_mutex_lock(&conn->dgram_lock);
                memcpy(conn->dgram_buffer, bytes, length);
                conn->dgram_len = length;
                conn->dgram_ready = 1;
                pthread_cond_signal(&conn->dgram_cond);
                pthread_mutex_unlock(&conn->dgram_lock);
            }
            break;

        case picoquic_callback_ready:
            fprintf(stderr, "DEBUG: Connection ready!\n");
            conn->handshake_done = 1;
            conn->is_connected = 1;
            break;

        case picoquic_callback_request_alpn_list:
            // Client side: add our ALPN to the list
            fprintf(stderr, "DEBUG: Adding ALPN 'l4s' to proposed list\n");
            if (bytes != NULL) {
                picoquic_add_proposed_alpn((void*)bytes, "l4s");
            }
            break;

        case picoquic_callback_set_alpn:
            // ALPN has been negotiated
            fprintf(stderr, "DEBUG: ALPN negotiated: %s\n", bytes ? (const char*)bytes : "(null)");
            break;

        case picoquic_callback_close:
        case picoquic_callback_application_close:
            fprintf(stderr, "DEBUG: Connection close callback received (event=%d)\n", event);
            pthread_mutex_lock(&conn->recv_lock);
            conn->is_closed = 1;
            conn->is_connected = 0;
            conn->cnx = NULL;  // Connection is being freed by picoquic
            pthread_cond_broadcast(&conn->recv_cond);
            pthread_mutex_unlock(&conn->recv_lock);
            pthread_mutex_lock(&conn->dgram_lock);
            pthread_cond_broadcast(&conn->dgram_cond);
            pthread_mutex_unlock(&conn->dgram_lock);
            break;

        default:
            break;
    }

    return 0;
}

// Packet loop callback - handles loop events
static int loop_callback(picoquic_quic_t* quic, picoquic_packet_loop_cb_enum cb_mode,
    void* callback_ctx, void* callback_arg) {

    pq_context_t* ctx = (pq_context_t*)callback_ctx;

    fprintf(stderr, "DEBUG: loop_callback mode=%d ctx=%p\n", cb_mode, (void*)ctx);

    if (!ctx) return 0;

    switch (cb_mode) {
        case picoquic_packet_loop_ready:
            // Socket is ready
            fprintf(stderr, "DEBUG: Socket ready\n");
            break;

        case picoquic_packet_loop_wake_up:
            // Process any pending sends for client connection
            if (ctx->client_conn) {
                process_send_queue(ctx->client_conn);
            }
            // Process sends for all connections
            {
                picoquic_cnx_t* cnx = picoquic_get_first_cnx(quic);
                while (cnx) {
                    void* cb_ctx = picoquic_get_callback_context(cnx);
                    // Check if it's a connection (not the context)
                    if (cb_ctx) {
                        uint32_t* magic = (uint32_t*)cb_ctx;
                        if (*magic == PQ_CONNECTION_MAGIC) {
                            pq_connection_t* conn = (pq_connection_t*)cb_ctx;
                            process_send_queue(conn);
                        }
                    }
                    cnx = picoquic_get_next_cnx(cnx);
                }
            }
            break;

        case picoquic_packet_loop_after_receive:
            // Check for new server connections
            if (ctx->is_server) {
                picoquic_cnx_t* cnx = picoquic_get_first_cnx(quic);
                while (cnx) {
                    void* cb_ctx = picoquic_get_callback_context(cnx);
                    int is_new_conn = 0;

                    // Check if this connection already has a wrapper
                    if (cb_ctx == NULL) {
                        is_new_conn = 1;
                    } else {
                        uint32_t* magic = (uint32_t*)cb_ctx;
                        // If it's the context (not a connection wrapper), it's new
                        if (*magic == PQ_CONTEXT_MAGIC) {
                            is_new_conn = 1;
                        }
                    }

                    if (is_new_conn && picoquic_get_cnx_state(cnx) >= picoquic_state_server_almost_ready) {
                        // New connection - create wrapper
                        fprintf(stderr, "DEBUG: Creating connection wrapper for new connection\n");

                        // Enable datagram support on server if client requested it
                        picoquic_tp_t const* tp = picoquic_get_transport_parameters(cnx, 1); // get local
                        picoquic_tp_t const* remote_tp = picoquic_get_transport_parameters(cnx, 0); // get remote
                        if (tp && remote_tp && remote_tp->max_datagram_frame_size > 0 && tp->max_datagram_frame_size == 0) {
                            picoquic_tp_t tp_copy = *tp;
                            tp_copy.max_datagram_frame_size = 1500;
                            picoquic_set_transport_parameters(cnx, &tp_copy);
                            fprintf(stderr, "DEBUG: Enabled datagram support on server (max_datagram_frame_size=1500)\n");
                        }

                        pq_connection_t* new_conn = create_connection(ctx, cnx);
                        if (new_conn) {
                            pthread_mutex_lock(&ctx->accept_lock);
                            ctx->pending_conn = new_conn;
                            ctx->has_pending_conn = 1;
                            pthread_cond_signal(&ctx->accept_cond);
                            pthread_mutex_unlock(&ctx->accept_lock);
                        }
                    }
                    cnx = picoquic_get_next_cnx(cnx);
                }
            }
            break;

        default:
            break;
    }

    return 0;
}

pq_context_t* pq_create_context(int is_server, const char* cert_file, const char* key_file) {
    pq_context_t* ctx = calloc(1, sizeof(pq_context_t));
    if (!ctx) return NULL;

    ctx->magic = PQ_CONTEXT_MAGIC;
    ctx->is_server = is_server;
    pthread_mutex_init(&ctx->accept_lock, NULL);
    pthread_cond_init(&ctx->accept_cond, NULL);

    uint64_t current_time = picoquic_current_time();

    if (is_server && cert_file && key_file) {
        ctx->quic = picoquic_create(8, cert_file, key_file, NULL, NULL,
            stream_callback, ctx, NULL, NULL, NULL, current_time, NULL,
            NULL, NULL, 0);
    } else {
        ctx->quic = picoquic_create(8, NULL, NULL, NULL, NULL,
            stream_callback, ctx, NULL, NULL, NULL, current_time, NULL,
            NULL, NULL, 0);
    }

    if (!ctx->quic) {
        free(ctx);
        return NULL;
    }

    // Set ALPN selector for server
    if (is_server) {
        picoquic_set_alpn_select_fn(ctx->quic, simple_alpn_select);
    }

    return ctx;
}

int pq_set_congestion_algorithm(pq_context_t* ctx, const char* algo_name) {
    if (!ctx || !ctx->quic) return -1;

    // Use the by_name function which handles all algorithm lookups internally
    picoquic_set_default_congestion_algorithm_by_name(ctx->quic, algo_name);
    return 0;
}

int pq_listen(pq_context_t* ctx, uint16_t port) {
    if (!ctx || !ctx->is_server) return -1;

    fprintf(stderr, "DEBUG: pq_listen starting on port %d\n", port);
    ctx->listen_port = port;

    // Start the network thread
    picoquic_packet_loop_param_t param = {0};
    param.local_port = port;
    param.local_af = AF_INET;

    fprintf(stderr, "DEBUG: Starting network thread for server\n");
    ctx->thread_ctx = picoquic_start_network_thread(ctx->quic, &param,
        loop_callback, ctx, &ctx->thread_ret);

    if (!ctx->thread_ctx) {
        fprintf(stderr, "DEBUG: Failed to start network thread\n");
        return -1;
    }
    fprintf(stderr, "DEBUG: Network thread started, waiting for ready, ret=%d\n", ctx->thread_ret);
    fprintf(stderr, "DEBUG: thread_ctx: ready=%d, closed=%d, should_close=%d\n",
            ctx->thread_ctx->thread_is_ready,
            ctx->thread_ctx->thread_is_closed,
            ctx->thread_ctx->thread_should_close);

    // Wait for thread to be ready with timeout
    int wait_count = 0;
    while (!ctx->thread_ctx->thread_is_ready && !ctx->thread_ctx->thread_is_closed) {
        usleep(1000);
        wait_count++;
        if (wait_count % 1000 == 0) {
            fprintf(stderr, "DEBUG: Still waiting, ready=%d, closed=%d, ret=%d\n",
                    ctx->thread_ctx->thread_is_ready,
                    ctx->thread_ctx->thread_is_closed,
                    ctx->thread_ctx->return_code);
        }
        if (wait_count > 10000) {
            fprintf(stderr, "DEBUG: Timeout waiting for thread ready\n");
            break;
        }
    }
    fprintf(stderr, "DEBUG: Thread ready=%d, closed=%d, ret=%d\n",
            ctx->thread_ctx->thread_is_ready, ctx->thread_ctx->thread_is_closed,
            ctx->thread_ctx->return_code);

    return 0;
}

pq_connection_t* pq_connect(pq_context_t* ctx, const char* server_name, uint16_t port) {
    if (!ctx || ctx->is_server) return NULL;

    struct sockaddr_storage server_addr;
    int server_addr_len = 0;

    fprintf(stderr, "DEBUG: Resolving %s:%d\n", server_name, port);

    // Resolve server address
    if (picoquic_get_server_address(server_name, port, &server_addr, &server_addr_len) != 0) {
        fprintf(stderr, "DEBUG: Failed to resolve server address\n");
        return NULL;
    }

    fprintf(stderr, "DEBUG: Server address resolved\n");

    uint64_t current_time = picoquic_current_time();

    // Create connection with "l4s" ALPN
    picoquic_cnx_t* cnx = picoquic_create_cnx(ctx->quic, picoquic_null_connection_id,
        picoquic_null_connection_id, (struct sockaddr*)&server_addr, current_time,
        0, server_name, "l4s", 1);

    if (!cnx) {
        fprintf(stderr, "DEBUG: Failed to create connection\n");
        return NULL;
    }
    fprintf(stderr, "DEBUG: Connection created\n");

    // Enable datagram support on client
    picoquic_tp_t const* tp = picoquic_get_transport_parameters(cnx, 1); // get local
    if (tp) {
        picoquic_tp_t tp_copy = *tp;
        tp_copy.max_datagram_frame_size = 1500; // Enable datagrams
        picoquic_set_transport_parameters(cnx, &tp_copy);
        fprintf(stderr, "DEBUG: Enabled datagram support (max_datagram_frame_size=1500)\n");
    }

    pq_connection_t* conn = create_connection(ctx, cnx);
    if (!conn) {
        picoquic_delete_cnx(cnx);
        return NULL;
    }

    ctx->client_conn = conn;

    // Start client connection
    picoquic_start_client_cnx(cnx);

    // Start the network thread
    picoquic_packet_loop_param_t param = {0};
    param.local_port = 0;  // Random local port
    param.local_af = AF_INET;

    fprintf(stderr, "DEBUG: Starting network thread\n");
    ctx->thread_ctx = picoquic_start_network_thread(ctx->quic, &param,
        loop_callback, ctx, &ctx->thread_ret);

    if (!ctx->thread_ctx) {
        fprintf(stderr, "DEBUG: Failed to start network thread\n");
        // Cleanup
        picoquic_delete_cnx(cnx);
        free(conn->recv_buffer);
        free(conn->dgram_buffer);
        free(conn);
        ctx->client_conn = NULL;
        return NULL;
    }
    fprintf(stderr, "DEBUG: Network thread started\n");

    // Wait for handshake to complete
    int timeout = 5000; // 5 second timeout
    while (!conn->handshake_done && !conn->is_closed && timeout > 0) {
        if (timeout % 1000 == 0) {
            picoquic_state_enum state = picoquic_get_cnx_state(cnx);
            fprintf(stderr, "DEBUG: Waiting for handshake, state=%d, done=%d, closed=%d\n",
                    state, conn->handshake_done, conn->is_closed);
        }
        usleep(1000);
        timeout--;
    }

    if (!conn->handshake_done) {
        picoquic_state_enum state = picoquic_get_cnx_state(cnx);
        fprintf(stderr, "DEBUG: Handshake failed, final state=%d\n", state);
        pq_close_connection(conn);
        return NULL;
    }

    fprintf(stderr, "DEBUG: Handshake completed\n");
    return conn;
}

pq_connection_t* pq_accept(pq_context_t* ctx) {
    if (!ctx || !ctx->is_server) return NULL;

    pthread_mutex_lock(&ctx->accept_lock);
    while (!ctx->has_pending_conn) {
        pthread_cond_wait(&ctx->accept_cond, &ctx->accept_lock);
    }

    pq_connection_t* conn = ctx->pending_conn;
    ctx->pending_conn = NULL;
    ctx->has_pending_conn = 0;
    pthread_mutex_unlock(&ctx->accept_lock);

    // Wait for handshake to complete
    int timeout = 5000;
    while (!conn->handshake_done && !conn->is_closed && timeout > 0) {
        usleep(1000);
        timeout--;
    }

    return conn;
}

int pq_send_stream(pq_connection_t* conn, const uint8_t* data, size_t len) {
    if (!conn || conn->is_closed) return -1;

    // Queue the data
    if (queue_send(conn, data, len, 1) != 0) {
        return -1;
    }

    // Wake up the network thread to process the send queue
    if (conn->ctx->thread_ctx) {
        picoquic_wake_up_network_thread(conn->ctx->thread_ctx);
    }

    return (int)len;
}

int pq_finish_stream(pq_connection_t* conn) {
    if (!conn || conn->is_closed) return -1;

    // Queue an empty send with FIN set
    if (queue_send_ex(conn, NULL, 0, 1, 1) != 0) {
        return -1;
    }

    // Wake up the network thread to process the send queue
    if (conn->ctx->thread_ctx) {
        picoquic_wake_up_network_thread(conn->ctx->thread_ctx);
    }

    return 0;
}

int pq_recv_stream(pq_connection_t* conn, uint8_t* buffer, size_t buffer_size) {
    if (!conn) return -1;

    pthread_mutex_lock(&conn->recv_lock);

    // Wait for data or FIN
    while (conn->recv_read >= conn->recv_write && !conn->is_closed && !conn->stream_fin_received) {
        pthread_cond_wait(&conn->recv_cond, &conn->recv_lock);
    }

    // Return EOF if no data and (closed or FIN received)
    if (conn->recv_read >= conn->recv_write && (conn->is_closed || conn->stream_fin_received)) {
        pthread_mutex_unlock(&conn->recv_lock);
        return 0;  // EOF
    }

    size_t available = conn->recv_write - conn->recv_read;
    size_t to_copy = available < buffer_size ? available : buffer_size;

    memcpy(buffer, conn->recv_buffer + conn->recv_read, to_copy);
    conn->recv_read += to_copy;

    // Reset buffer if fully consumed
    if (conn->recv_read >= conn->recv_write) {
        conn->recv_read = 0;
        conn->recv_write = 0;
    }

    pthread_mutex_unlock(&conn->recv_lock);

    return (int)to_copy;
}

int pq_send_datagram(pq_connection_t* conn, const uint8_t* data, size_t len) {
    if (!conn || conn->is_closed) return -1;

    if (queue_send(conn, data, len, 0) != 0) {
        return -1;
    }

    if (conn->ctx->thread_ctx) {
        picoquic_wake_up_network_thread(conn->ctx->thread_ctx);
    }

    return (int)len;
}

int pq_recv_datagram(pq_connection_t* conn, uint8_t* buffer, size_t buffer_size) {
    if (!conn) return -1;

    pthread_mutex_lock(&conn->dgram_lock);

    while (!conn->dgram_ready && !conn->is_closed) {
        pthread_cond_wait(&conn->dgram_cond, &conn->dgram_lock);
    }

    if (conn->is_closed && !conn->dgram_ready) {
        pthread_mutex_unlock(&conn->dgram_lock);
        return -1;
    }

    size_t to_copy = conn->dgram_len < buffer_size ? conn->dgram_len : buffer_size;
    memcpy(buffer, conn->dgram_buffer, to_copy);
    conn->dgram_ready = 0;

    pthread_mutex_unlock(&conn->dgram_lock);

    return (int)to_copy;
}

void pq_close_connection(pq_connection_t* conn) {
    if (!conn) return;

    // Only close the QUIC connection if not already closed
    pthread_mutex_lock(&conn->recv_lock);
    if (!conn->is_closed && conn->cnx) {
        conn->is_closed = 1;
        conn->is_connected = 0;
        picoquic_cnx_t* cnx = conn->cnx;
        conn->cnx = NULL;  // Prevent double-close
        pthread_mutex_unlock(&conn->recv_lock);
        picoquic_close(cnx, 0);
    } else {
        conn->is_closed = 1;
        conn->is_connected = 0;
        conn->cnx = NULL;
        pthread_mutex_unlock(&conn->recv_lock);
    }

    // Wake up any waiting threads
    pthread_mutex_lock(&conn->recv_lock);
    pthread_cond_broadcast(&conn->recv_cond);
    pthread_mutex_unlock(&conn->recv_lock);

    pthread_mutex_lock(&conn->dgram_lock);
    pthread_cond_broadcast(&conn->dgram_cond);
    pthread_mutex_unlock(&conn->dgram_lock);
}

void pq_destroy_context(pq_context_t* ctx) {
    if (!ctx) return;

    // Stop the network thread
    if (ctx->thread_ctx) {
        picoquic_delete_network_thread(ctx->thread_ctx);
    }

    if (ctx->quic) {
        picoquic_free(ctx->quic);
    }

    pthread_mutex_destroy(&ctx->accept_lock);
    pthread_cond_destroy(&ctx->accept_cond);

    free(ctx);
}

int pq_is_connected(pq_connection_t* conn) {
    if (!conn) return 0;
    return conn->is_connected && !conn->is_closed;
}

// Not needed with threaded loop, but keep for API compatibility
int pq_process_events(pq_context_t* ctx, int timeout_ms) {
    (void)ctx;
    usleep(timeout_ms * 1000);
    return 0;
}
