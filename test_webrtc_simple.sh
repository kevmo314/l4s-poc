#!/bin/bash
set -e

PORT=9096

# Start receiver in background
echo "TestWebRTCData" | timeout 15 ./webrtc-recv --http-addr :$PORT -v > /tmp/wr_video.h264 2>/tmp/wr_recv.log &
RECV_PID=$!

sleep 2

# Run sender
timeout 10 ./webrtc-send --whip-url http://127.0.0.1:$PORT/whip -v --dc-timeout 3s < test_long.h264 > /tmp/wr_dc.bin 2>/tmp/wr_send.log || true

sleep 2

# Kill receiver
kill $RECV_PID 2>/dev/null || true

echo "=== Sender log ==="
cat /tmp/wr_send.log

echo ""
echo "=== Receiver log ==="
cat /tmp/wr_recv.log

echo ""
echo "=== Results ==="
echo "Video: $(wc -c < /tmp/wr_video.h264) bytes"
echo "DC: $(wc -c < /tmp/wr_dc.bin) bytes"
