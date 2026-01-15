# L4S Video Streaming PoC

Proof of concept for video streaming with L4S (Low Latency, Low Loss, Scalable Throughput) support over QUIC and WebRTC.

## Build

### Docker (recommended)

```bash
docker build -t l4s-benchmark .
```

### Native

Requires building the third-party C libraries first:

```bash
# Clone and build picotls
git clone https://github.com/pion/picotls third_party/picotls
cd third_party/picotls && git submodule update --init
cmake -B build && cmake --build build
cd ../..

# Clone and build picoquic (with Prague CC)
git clone https://github.com/pion/picoquic third_party/picoquic
cd third_party/picoquic
cmake -B build -DPICOTLS_INCLUDE_DIR=../picotls/include \
      -DPICOTLS_CORE_LIBRARY=../picotls/build/libpicotls-core.a \
      -DPICOTLS_FUSION_LIBRARY=../picotls/build/libpicotls-fusion.a \
      -DPICOTLS_OPENSSL_LIBRARY=../picotls/build/libpicotls-openssl.a
cmake --build build
cd ../..

# Clone and build SCReAM
git clone https://github.com/pion/scream third_party/scream
cd third_party/scream/code/wrapper_lib
cmake -B build && cmake --build build
cd ../../../..

# Build Go binaries
CGO_ENABLED=1 go build -o bin/webrtc-send ./cmd/webrtc-send
CGO_ENABLED=1 go build -o bin/webrtc-recv ./cmd/webrtc-recv
CGO_ENABLED=1 go build -o bin/quic-send ./cmd/quic-send
CGO_ENABLED=1 go build -o bin/quic-recv ./cmd/quic-recv
```

See `Dockerfile` for complete build steps including CGO flags.

## Usage

### WebRTC

```bash
# Receiver (on server with public IP, e.g., GCP)
docker run --rm --network=host l4s-benchmark webrtc-recv --http-addr :8080 -v

# Sender
ffmpeg -f lavfi -i testsrc=duration=10:size=1280x720:rate=30 -c:v libx264 -f h264 pipe:1 | \
  docker run --rm -i l4s-benchmark webrtc-send --whip-url http://<receiver-ip>:8080/whip -v
```

### QUIC (with Prague congestion control)

```bash
# Receiver
docker run --rm --network=host l4s-benchmark \
  quic-recv --port 5000 --cert /app/certs/cert.pem --key /app/certs/key.pem -v

# Sender
ffmpeg -f lavfi -i testsrc=duration=10:size=1280x720:rate=30 -c:v libx264 -f h264 pipe:1 | \
  docker run --rm -i l4s-benchmark quic-send --addr <receiver-ip>:5000 -v
```

## Cloud Deployment

The WebRTC receiver auto-detects its public IP on GCP via the metadata server and configures NAT 1:1 mapping automatically.
