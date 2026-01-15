#!/bin/bash
# Test QUIC bidirectional data channel

PORT=5003

# Generate side channel data (10KB)
dd if=/dev/urandom of=/tmp/side_channel_data.bin bs=1024 count=10 2>/dev/null

# Start receiver with stdin data for side channel
cat /tmp/side_channel_data.bin | ./quic-recv --port $PORT --cert certs/cert.pem --key certs/key.pem -v > /tmp/quic_video_out.h264 2>/tmp/quic_recv.log &
RECV_PID=$!
sleep 3

# Send video and capture data channel output from receiver
cat test_long.h264 | ./quic-send --addr 127.0.0.1:$PORT -v --dc-timeout 5s > /tmp/quic_dc_out.bin 2>/tmp/quic_send.log

# Wait for receiver to finish
wait $RECV_PID 2>/dev/null || true

echo "=== Receiver log ==="
cat /tmp/quic_recv.log
echo ""
echo "=== Sender log ==="
cat /tmp/quic_send.log
echo ""
echo "=== Data channel output size ==="
ls -la /tmp/quic_dc_out.bin
echo ""
echo "=== Video comparison ==="
echo "Original: $(wc -c < test_long.h264) bytes"
echo "Received: $(wc -c < /tmp/quic_video_out.h264) bytes"
echo ""
echo "=== Side channel comparison ==="
echo "Sent: $(wc -c < /tmp/side_channel_data.bin) bytes"
echo "Received: $(wc -c < /tmp/quic_dc_out.bin) bytes"
