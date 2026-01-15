#!/bin/bash
# Test WebRTC bidirectional data channel

# Generate side channel data (10KB)
dd if=/dev/urandom of=/tmp/side_channel_data.bin bs=1024 count=10 2>/dev/null

# Start receiver with stdin data for side channel
cat /tmp/side_channel_data.bin | ./webrtc-recv --http-addr :9091 -v > /tmp/webrtc_video_out.h264 2>/tmp/webrtc_recv.log &
RECV_PID=$!
sleep 2

# Send video and capture data channel output from receiver
cat test_long.h264 | ./webrtc-send --whip-url http://127.0.0.1:9091/whip -v --dc-timeout 3s > /tmp/webrtc_dc_out.bin 2>/tmp/webrtc_send.log

# Wait for receiver to finish
sleep 2
kill $RECV_PID 2>/dev/null || true

echo "=== Key receiver log lines ==="
grep -E "(Finished|Error|DataChannel|Data channel)" /tmp/webrtc_recv.log
echo ""
echo "=== Key sender log lines ==="
grep -E "(Finished|Error|DataChannel|Data channel)" /tmp/webrtc_send.log
echo ""
echo "=== Video comparison ==="
echo "Original: $(wc -c < test_long.h264) bytes, $(./quic-recv --port 9999 --cert certs/cert.pem --key certs/key.pem -v < test_long.h264 2>&1 | grep -o '[0-9]* NALs' | head -1) NALs"
echo "Received: $(wc -c < /tmp/webrtc_video_out.h264) bytes"
echo ""
echo "=== Side channel comparison ==="
echo "Sent: $(wc -c < /tmp/side_channel_data.bin) bytes"
echo "Received: $(wc -c < /tmp/webrtc_dc_out.bin) bytes"
