# L4S Benchmark Suite Dockerfile
# Builds QUIC+L4S and WebRTC+L4S sender/receiver binaries

# Build stage
FROM golang:1.23-bookworm AS builder

# Install build dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    cmake \
    git \
    libssl-dev \
    pkg-config \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Detect architecture for conditional builds
ARG TARGETARCH

# Clone and build picotls (required by picoquic)
# Note: fusion library is x86-only (uses SSE/AES-NI), skip on ARM64
RUN git clone --depth 1 https://github.com/h2o/picotls.git third_party/picotls && \
    cd third_party/picotls && \
    git submodule init && \
    git submodule update --depth 1 && \
    if [ "$TARGETARCH" = "arm64" ]; then \
        cmake . -DCMAKE_BUILD_TYPE=Release -DWITH_FUSION=OFF; \
    else \
        cmake . -DCMAKE_BUILD_TYPE=Release; \
    fi && \
    make -j$(nproc)

# Clone and build picoquic with Prague congestion control
RUN git clone --depth 1 --recurse-submodules https://github.com/private-octopus/picoquic.git third_party/picoquic && \
    cd third_party/picoquic && \
    if [ "$TARGETARCH" = "arm64" ]; then \
        cmake . -DCMAKE_BUILD_TYPE=Release \
            -DPTLS_INCLUDE_DIR=/build/third_party/picotls/include \
            -DPTLS_CORE_LIBRARY=/build/third_party/picotls/libpicotls-core.a \
            -DPTLS_OPENSSL_LIBRARY=/build/third_party/picotls/libpicotls-openssl.a \
            -DPTLS_MINICRYPTO_LIBRARY=/build/third_party/picotls/libpicotls-minicrypto.a; \
    else \
        cmake . -DCMAKE_BUILD_TYPE=Release \
            -DPTLS_INCLUDE_DIR=/build/third_party/picotls/include \
            -DPTLS_CORE_LIBRARY=/build/third_party/picotls/libpicotls-core.a \
            -DPTLS_OPENSSL_LIBRARY=/build/third_party/picotls/libpicotls-openssl.a \
            -DPTLS_MINICRYPTO_LIBRARY=/build/third_party/picotls/libpicotls-minicrypto.a \
            -DPTLS_FUSION_LIBRARY=/build/third_party/picotls/libpicotls-fusion.a; \
    fi && \
    make -j$(nproc)

# Clone and build SCReAM congestion control
RUN git clone --depth 1 https://github.com/EricssonResearch/scream.git third_party/scream && \
    cd third_party/scream/code/wrapper_lib && \
    cmake . -DCMAKE_BUILD_TYPE=Release && \
    make -j$(nproc)

# Copy Go source files
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

# Set CGO flags for building with picoquic and scream
ENV CGO_ENABLED=1
ENV CGO_CFLAGS="-I/build/third_party/picoquic -I/build/third_party/picoquic/picoquic -I/build/third_party/picotls/include -I/build/third_party/scream/code -I/build/third_party/scream/code/wrapper_lib"

# Build all binaries (conditionally link fusion library on x86 only)
RUN mkdir -p bin && \
    if [ "$TARGETARCH" = "arm64" ]; then \
        export CGO_LDFLAGS="-L/build/third_party/picoquic -L/build/third_party/picotls -L/build/third_party/scream/code/wrapper_lib -lpicoquic-core -lpicoquic-log -lpicotls-core -lpicotls-openssl -lssl -lcrypto -lm -lstdc++"; \
    else \
        export CGO_LDFLAGS="-L/build/third_party/picoquic -L/build/third_party/picotls -L/build/third_party/scream/code/wrapper_lib -lpicoquic-core -lpicoquic-log -lpicotls-core -lpicotls-openssl -lpicotls-fusion -lssl -lcrypto -lm -lstdc++"; \
    fi && \
    go build -o bin/quic-send ./cmd/quic-send && \
    go build -o bin/quic-recv ./cmd/quic-recv && \
    go build -o bin/webrtc-send ./cmd/webrtc-send && \
    go build -o bin/webrtc-recv ./cmd/webrtc-recv

# Generate self-signed certificate for QUIC
RUN mkdir -p certs && \
    openssl req -x509 -newkey rsa:2048 -keyout certs/key.pem -out certs/cert.pem \
        -days 365 -nodes -subj "/CN=localhost"

# Runtime stage
FROM debian:bookworm-slim AS runtime

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    libssl3 \
    ca-certificates \
    ffmpeg \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /build/bin/ /app/bin/
COPY --from=builder /build/certs/ /app/certs/

# Copy scream shared library if it exists (for CGO builds that use it)
# Note: Current binaries are statically linked so this may not be needed
RUN --mount=from=builder,source=/build/third_party/scream/code/wrapper_lib,target=/tmp/scream \
    cp /tmp/scream/libscream.so /usr/local/lib/ 2>/dev/null || true && \
    ldconfig 2>/dev/null || true

# Add binaries to PATH
ENV PATH="/app/bin:${PATH}"

# Default port for QUIC receiver
EXPOSE 5000/udp
# Default port for WebRTC receiver
EXPOSE 8080/tcp

# Create a simple entrypoint script
RUN echo '#!/bin/bash\n\
echo "L4S Benchmark Suite"\n\
echo "=================="\n\
echo ""\n\
echo "Available binaries:"\n\
echo "  quic-send    - Send H264 video via QUIC with L4S/Prague"\n\
echo "  quic-recv    - Receive H264 video via QUIC"\n\
echo "  webrtc-send  - Send H264 video via WebRTC"\n\
echo "  webrtc-recv  - Receive H264 video via WebRTC (WHIP server)"\n\
echo ""\n\
echo "Certificates in /app/certs/"\n\
echo ""\n\
echo "Example usage:"\n\
echo "  # QUIC receiver:"\n\
echo "  quic-recv --port 5000 --cert /app/certs/cert.pem --key /app/certs/key.pem"\n\
echo ""\n\
echo "  # QUIC sender:"\n\
echo "  cat video.h264 | quic-send --addr receiver:5000"\n\
echo ""\n\
echo "  # WebRTC receiver:"\n\
echo "  webrtc-recv --http-addr :8080"\n\
echo ""\n\
echo "  # WebRTC sender:"\n\
echo "  cat video.h264 | webrtc-send --whip-url http://receiver:8080/whip"\n\
echo ""\n\
if [ $# -gt 0 ]; then\n\
    exec "$@"\n\
fi\n' > /app/entrypoint.sh && chmod +x /app/entrypoint.sh

ENTRYPOINT ["/app/entrypoint.sh"]
