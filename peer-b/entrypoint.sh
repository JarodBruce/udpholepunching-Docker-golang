#!/bin/sh

# Set the default policy for the INPUT chain to DROP.
# This means that any incoming packet that does not match a rule will be dropped.
echo "Setting default INPUT policy to DROP"
iptables -P INPUT DROP

# Allow traffic on the loopback interface for local communication.
iptables -A INPUT -i lo -j ACCEPT

# Allow established and related incoming connections.
# This is crucial for the return traffic of the UDP hole punching to be accepted.
iptables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

# List the current iptables rules for verification.
echo "Current iptables rules:"
iptables -L

# Execute the main application.
echo "Starting peer application"
exec /usr/local/bin/peer "$@"
