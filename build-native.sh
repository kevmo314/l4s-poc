#!/bin/bash
set -e

# L4S Native Build Script
# Builds picotls, picoquic (with Prague CC), SCReAM, and Go binaries

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Detect architecture
ARCH=$(uname -m)
echo "Building for architecture: $ARCH"

# Check for required dependencies
echo "Checking dependencies..."
for cmd in git cmake make go openssl pkg-config; do
    if ! command -v $cmd &> /dev/null; then
        echo "Error: $cmd is required but not installed."
        exit 1
    fi
done

# Check for OpenSSL development headers
if ! pkg-config --exists openssl 2>/dev/null; then
    echo "Warning: OpenSSL development headers may not be installed."
    echo "On Debian/Ubuntu: apt-get install libssl-dev"
    echo "On macOS: brew install openssl"
fi

mkdir -p third_party bin

# Clone and build picotls
echo ""
echo "=== Building picotls ==="
if [ ! -d "third_party/picotls" ]; then
    git clone --depth 1 https://github.com/h2o/picotls.git third_party/picotls
    cd third_party/picotls
    git submodule init
    git submodule update --depth 1
else
    echo "picotls already cloned, updating..."
    cd third_party/picotls
    git pull --depth 1 || true
    git submodule update --depth 1
fi

# Build picotls (skip fusion on ARM64 - it uses x86 SSE/AES-NI)
if [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    cmake . -DCMAKE_BUILD_TYPE=Release -DWITH_FUSION=OFF
else
    cmake . -DCMAKE_BUILD_TYPE=Release
fi
make -j$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
cd "$SCRIPT_DIR"

# Clone and build picoquic
echo ""
echo "=== Building picoquic ==="
if [ ! -d "third_party/picoquic" ]; then
    git clone --depth 1 --recurse-submodules https://github.com/private-octopus/picoquic.git third_party/picoquic
else
    echo "picoquic already cloned, updating..."
    cd third_party/picoquic
    git pull --depth 1 || true
    git submodule update --depth 1
    cd "$SCRIPT_DIR"
fi

cd third_party/picoquic
if [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    cmake . -DCMAKE_BUILD_TYPE=Release \
        -DPTLS_INCLUDE_DIR="$SCRIPT_DIR/third_party/picotls/include" \
        -DPTLS_CORE_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-core.a" \
        -DPTLS_OPENSSL_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-openssl.a" \
        -DPTLS_MINICRYPTO_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-minicrypto.a"
else
    cmake . -DCMAKE_BUILD_TYPE=Release \
        -DPTLS_INCLUDE_DIR="$SCRIPT_DIR/third_party/picotls/include" \
        -DPTLS_CORE_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-core.a" \
        -DPTLS_OPENSSL_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-openssl.a" \
        -DPTLS_MINICRYPTO_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-minicrypto.a" \
        -DPTLS_FUSION_LIBRARY="$SCRIPT_DIR/third_party/picotls/libpicotls-fusion.a"
fi
make -j$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
cd "$SCRIPT_DIR"

# Clone and build SCReAM
echo ""
echo "=== Building SCReAM ==="
if [ ! -d "third_party/scream" ]; then
    git clone --depth 1 https://github.com/EricssonResearch/scream.git third_party/scream
else
    echo "scream already cloned, updating..."
    cd third_party/scream
    git pull --depth 1 || true
    cd "$SCRIPT_DIR"
fi

cd third_party/scream/code/wrapper_lib
cmake . -DCMAKE_BUILD_TYPE=Release
make -j$(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4)
cd "$SCRIPT_DIR"

# Set up CGO environment
echo ""
echo "=== Building Go binaries ==="
export CGO_ENABLED=1
export CGO_CFLAGS="-I$SCRIPT_DIR/third_party/picoquic -I$SCRIPT_DIR/third_party/picoquic/picoquic -I$SCRIPT_DIR/third_party/picotls/include -I$SCRIPT_DIR/third_party/scream/code -I$SCRIPT_DIR/third_party/scream/code/wrapper_lib"

if [ "$ARCH" = "aarch64" ] || [ "$ARCH" = "arm64" ]; then
    export CGO_LDFLAGS="-L$SCRIPT_DIR/third_party/picoquic -L$SCRIPT_DIR/third_party/picotls -L$SCRIPT_DIR/third_party/scream/code/wrapper_lib -lpicoquic-core -lpicoquic-log -lpicotls-core -lpicotls-openssl -lssl -lcrypto -lm -lstdc++"
else
    export CGO_LDFLAGS="-L$SCRIPT_DIR/third_party/picoquic -L$SCRIPT_DIR/third_party/picotls -L$SCRIPT_DIR/third_party/scream/code/wrapper_lib -lpicoquic-core -lpicoquic-log -lpicotls-core -lpicotls-openssl -lpicotls-fusion -lssl -lcrypto -lm -lstdc++"
fi

go build -o bin/quic-send ./cmd/quic-send
go build -o bin/quic-recv ./cmd/quic-recv
go build -o bin/webrtc-send ./cmd/webrtc-send
go build -o bin/webrtc-recv ./cmd/webrtc-recv

# Generate self-signed certificate for QUIC
echo ""
echo "=== Generating certificates ==="
mkdir -p certs
if [ ! -f "certs/cert.pem" ]; then
    openssl req -x509 -newkey rsa:2048 -keyout certs/key.pem -out certs/cert.pem \
        -days 365 -nodes -subj "/CN=localhost"
    echo "Generated new certificates in certs/"
else
    echo "Certificates already exist in certs/"
fi

echo ""
echo "=== Build complete ==="
echo "Binaries are in bin/"
ls -la bin/
