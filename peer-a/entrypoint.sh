#!/bin/sh
set -e

# Optional: wait for the other peer to be resolvable
if [ -n "$REMOTE_ADDR" ]; then
    OTHER_PEER=$(echo "$REMOTE_ADDR" | cut -d: -f1)
    echo "Waiting for $OTHER_PEER to be ready..."
    # Simple loop to wait for DNS resolution
    for i in 1 2 3 4 5; do
        if nslookup "$OTHER_PEER" >/dev/null 2>&1; then
            echo "$OTHER_PEER is resolvable."
            break
        fi
        echo "Attempt $i: $OTHER_PEER not found, sleeping..."
        sleep 2
    done
fi

# Set the default policy for the INPUT chain to ACCEPT.
echo "Setting default INPUT policy to ACCEPT"
iptables -P INPUT ACCEPT

# List the current iptables rules for verification.
echo "Current iptables rules:"
iptables -L

# Execute the main application.
echo "Starting peer application"
exec /usr/local/bin/peer "$@"
