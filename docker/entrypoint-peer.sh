#!/bin/bash
set -e

# Apply netem WAN simulation if NET_ADMIN capability is available.
if [ -n "$WAN_DELAY" ] || [ -n "$WAN_JITTER" ] || [ -n "$WAN_LOSS" ]; then
    tc qdisc add dev eth0 root netem \
        delay ${WAN_DELAY:-50ms} ${WAN_JITTER:-10ms} \
        loss ${WAN_LOSS:-1%} 2>/dev/null || true
    echo "Netem configured: delay=${WAN_DELAY:-50ms} jitter=${WAN_JITTER:-10ms} loss=${WAN_LOSS:-1%}"
fi

# Build command args — conditionally add optional flags.
ARGS=("$@")

if [ -n "$NOTE_SCRIPT" ]; then
    ARGS+=("--note-script" "$NOTE_SCRIPT")
fi

if [ -n "$EXPECT_NOTES" ]; then
    ARGS+=("--expect-notes" "$EXPECT_NOTES")
fi

if [ -n "$CHAOS_SCRIPT" ]; then
    ARGS+=("--chaos-script" "$CHAOS_SCRIPT")
fi

if [ -n "$VALIDATE_PEER" ]; then
    ARGS+=("--validate-peer" "$VALIDATE_PEER")
fi

# Use stdbuf to force line-buffered stdout so Docker logs appear in real time.
exec stdbuf -oL wail-test-client "${ARGS[@]}"
