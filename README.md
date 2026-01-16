# L4S Video Streaming PoC

Proof of concept for video streaming with L4S (Low Latency, Low Loss, Scalable Throughput) support over QUIC and WebRTC.

## Build

### Docker (recommended)

```bash
docker build -t l4s-benchmark .
```

### Native

Requires git, cmake, make, go, openssl, and libssl-dev (or equivalent).

```bash
./build-native.sh
```

This script clones and builds:
- [picotls](https://github.com/h2o/picotls) - TLS 1.3 library
- [picoquic](https://github.com/private-octopus/picoquic) - QUIC with Prague congestion control
- [SCReAM](https://github.com/EricssonResearch/scream) - Congestion control for real-time media

Binaries are output to `bin/`. Self-signed certificates are generated in `certs/`.

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
