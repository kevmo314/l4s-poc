.PHONY: all clean deps deps-picotls deps-picoquic deps-scream build test

# Directories
THIRD_PARTY := third_party
PICOTLS_DIR := $(THIRD_PARTY)/picotls
PICOQUIC_DIR := $(THIRD_PARTY)/picoquic
SCREAM_DIR := $(THIRD_PARTY)/scream

# Build flags for CGO
export CGO_CFLAGS := -I$(CURDIR)/$(PICOQUIC_DIR) -I$(CURDIR)/$(PICOQUIC_DIR)/picoquic -I$(CURDIR)/$(PICOTLS_DIR)/include -I$(CURDIR)/$(SCREAM_DIR)/code -I$(CURDIR)/$(SCREAM_DIR)/code/wrapper_lib
export CGO_LDFLAGS := -L$(CURDIR)/$(PICOQUIC_DIR) -L$(CURDIR)/$(PICOTLS_DIR) -L$(CURDIR)/$(SCREAM_DIR)/code/wrapper_lib -lpicoquic-core -lpicoquic-log -lpicotls-core -lpicotls-openssl -lpicotls-fusion -lscream -lssl -lcrypto -lm -lstdc++
export LD_LIBRARY_PATH := $(CURDIR)/$(SCREAM_DIR)/code/wrapper_lib:$(LD_LIBRARY_PATH)

all: deps build

# Clone and build picotls (required by picoquic)
deps-picotls:
	@if [ ! -d "$(PICOTLS_DIR)" ]; then \
		echo "Cloning picotls..."; \
		git clone --depth 1 https://github.com/h2o/picotls.git $(PICOTLS_DIR); \
		cd $(PICOTLS_DIR) && git submodule init && git submodule update --depth 1; \
	fi
	@echo "Building picotls..."
	cd $(PICOTLS_DIR) && cmake . -DCMAKE_BUILD_TYPE=Release && make -j$$(nproc)

# Clone and build picoquic
deps-picoquic: deps-picotls
	@if [ ! -d "$(PICOQUIC_DIR)" ]; then \
		echo "Cloning picoquic..."; \
		git clone --depth 1 --recurse-submodules https://github.com/private-octopus/picoquic.git $(PICOQUIC_DIR); \
	fi
	@echo "Building picoquic..."
	cd $(PICOQUIC_DIR) && cmake . -DCMAKE_BUILD_TYPE=Release \
		-DPTLS_INCLUDE_DIR=$(CURDIR)/$(PICOTLS_DIR)/include \
		-DPTLS_CORE_LIBRARY=$(CURDIR)/$(PICOTLS_DIR)/libpicotls-core.a \
		-DPTLS_OPENSSL_LIBRARY=$(CURDIR)/$(PICOTLS_DIR)/libpicotls-openssl.a \
		-DPTLS_MINICRYPTO_LIBRARY=$(CURDIR)/$(PICOTLS_DIR)/libpicotls-minicrypto.a \
		-DPTLS_FUSION_LIBRARY=$(CURDIR)/$(PICOTLS_DIR)/libpicotls-fusion.a \
		&& make -j$$(nproc)

# Clone and build SCReAM
deps-scream:
	@if [ ! -d "$(SCREAM_DIR)" ]; then \
		echo "Cloning SCReAM..."; \
		git clone --depth 1 https://github.com/EricssonResearch/scream.git $(SCREAM_DIR); \
	fi
	@echo "Building SCReAM..."
	cd $(SCREAM_DIR)/code/wrapper_lib && cmake . -DCMAKE_BUILD_TYPE=Release && make -j$$(nproc)

deps: deps-picoquic deps-scream
	go mod tidy

build:
	mkdir -p bin
	go build -o bin/quic-send ./cmd/quic-send
	go build -o bin/quic-recv ./cmd/quic-recv
	go build -o bin/webrtc-send ./cmd/webrtc-send
	go build -o bin/webrtc-recv ./cmd/webrtc-recv

# Generate test H264 file
test.h264:
	ffmpeg -f lavfi -i testsrc=duration=10:size=1280x720:rate=30 -c:v libx264 -f h264 test.h264

test: test.h264 build
	@echo "Running QUIC loopback test..."
	./bin/quic-recv --listen 0.0.0.0:5000 > output.h264 &
	sleep 1
	cat test.h264 | ./bin/quic-send --addr 127.0.0.1:5000
	sleep 2
	pkill -f "quic-recv" || true
	diff test.h264 output.h264 && echo "QUIC test PASSED" || echo "QUIC test FAILED"
	rm -f output.h264

clean:
	rm -rf bin/ test.h264 output.h264

clean-deps:
	rm -rf $(THIRD_PARTY)
