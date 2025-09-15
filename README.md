# UDP Hole Punching with WebRTC File Transfer

P2P file transfer using UDP hole punching and WebRTC DataChannels with multi-channel parallel transfer for speed.

## Features

- **Multi-channel parallel transfer**: Uses multiple WebRTC DataChannels for faster file transfer
- **Windows-safe defaults**: Automatic localhost binding and reduced AV heuristics on Windows
- **Interactive prompts**: Ask for remote peer address at startup (can be disabled)
- **Docker support**: Complete containerized setup with proper networking
- **Progress tracking**: Real-time transfer progress with speed and ETA

## Quick Start

### Docker (Recommended)

```bash
# Clone and navigate to the project
git clone <repo-url>
cd udpholepunching

# Start both peers (peer-a will send data/sample.txt to peer-b)
docker compose up --build

# To send a different file, override the command:
# docker compose run peer-a /data/your-file.txt
```

### Manual Build

```bash
# Build both peers
cd peer-a && go build -o peer-a .
cd ../peer-b && go build -o peer-b .

# Run receiver first
./peer-b/peer-b

# Run sender with file (in another terminal)
./peer-a/peer-a /path/to/file.txt
```

## Environment Variables

### Network Configuration

- `REMOTE_ADDR`: Remote peer address (e.g., `192.168.1.100:8080`)
- `LOCAL_ADDR` or `LOCAL_PORT`: Local bind address/port
- `BIND_LOCALHOST_ONLY`: `1` to bind only to localhost (default on Windows)

### Transfer Tuning

- `CHANNELS`: Number of parallel DataChannels (default: 4)
- `RECV_DIR`: Output directory for received files (default: `data/`)

### Behavior Control

- `PROMPT_DISABLE`: `1` to skip interactive address prompts
- `DISABLE_PUNCH`: `1` to disable UDP NAT punching (default on Windows)

## Windows Anti-Virus Mitigation

### Built-in Protections

- **Localhost-only binding**: Windows defaults to `127.0.0.1` binding to avoid firewall prompts
- **Disabled UDP punching**: Reduces heuristic triggers in Windows Defender
- **No external connections**: Safe for local testing without AV interference

### For Production Use

1. **Code Signing**: Use Authenticode certificates to sign binaries
2. **Version Information**: Embed FileVersion, ProductName, CompanyName
3. **Distribution**: Package in ZIP with clear source attribution
4. **Defender Submission**: Submit false positives to Microsoft Security Intelligence

### Example Windows-Safe Usage

```bash
# Local testing (both peers on same machine)
set BIND_LOCALHOST_ONLY=1
set PROMPT_DISABLE=1
peer-b.exe
# In another terminal:
peer-a.exe C:\path\to\file.txt

# For LAN use, explicitly enable external binding
set BIND_LOCALHOST_ONLY=0
set REMOTE_ADDR=192.168.1.100:8080
```

## Architecture

### Connection Flow

1. **UDP Handshake**: Session ID exchange and WebRTC offer/answer via UDP
2. **DataChannel Setup**: Multiple channels created (default: 4)
3. **Metadata Exchange**: File info sent, receiver confirms READY
4. **Parallel Transfer**: File chunks sent across multiple channels with framing
5. **Reassembly**: Receiver uses `WriteAt` for random-access reconstruction

### Frame Format (v2)

```
[8 bytes: file offset (little-endian)]
[4 bytes: payload length (little-endian)]
[payload data]
```

### Fallback Compatibility

- Version 1: Single channel, sequential writes
- Version 2: Multi-channel with framed offsets
- Automatic detection based on metadata `ver` field

## Troubleshooting

### Docker Issues

- **Containers hang on startup**: Check `PROMPT_DISABLE=1` is set
- **Connection timeouts**: Verify container network configuration
- **Permission errors**: Ensure `cap_add: NET_ADMIN` for iptables

### Transfer Problems

- **Slow speeds**: Increase `CHANNELS` (try 8-16 for high-bandwidth links)
- **Connection failures**: Check firewall/NAT configuration
- **File corruption**: Verify received file size matches expected

### Windows Defender

- **Binary deleted**: Use localhost-only mode or add to exclusions
- **False positive reports**: Submit samples to Microsoft
- **Corporate environments**: Work with IT for policy exceptions

## Development

### Adding Features

- Modify `peer-a/main.go` for sender logic
- Modify `peer-b/main.go` for receiver logic
- Update `docker-compose.yml` for new environment variables

### Performance Tuning

- Adjust `chunkSize` (16KB default) for latency vs. throughput
- Tune `maxBufferedBytes` (16MB default) for memory vs. speed
- Experiment with `CHANNELS` count based on link characteristics

## License

[Add your license here]
