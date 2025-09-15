# Use the official Golang image
FROM golang:1.22-alpine

# Install iptables
RUN apk add --no-cache iptables

# Set the working directory
WORKDIR /usr/src/app

# Copy the entrypoint script and Go source code
COPY entrypoint.sh .
COPY main.go .

# Make the entrypoint script executable
RUN chmod +x entrypoint.sh

# Build the Go application
RUN go build -o /usr/local/bin/peer main.go

# Set the entrypoint
ENTRYPOINT ["./entrypoint.sh"]
