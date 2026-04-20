FROM golang:1.26 AS builder

WORKDIR /app

# Install dependencies if you have them (go.mod, go.sum exist)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -buildvcs=false -o kula ./cmd/kula/

# ---- Final Stage ----
FROM alpine:3.21

# Create unprivileged user
RUN addgroup -S kula && adduser -S kula -G kula

WORKDIR /app

# Install required tools (e.g., bash for completion script if used, or tzdata)
RUN apk --no-cache add ca-certificates tzdata bash

# Copy the binary from the builder stage
COPY --from=builder /app/kula /app/kula

# Copy example config and adjust it for Docker (/app/data and listen on 0.0.0.0)
COPY config.example.yaml /app/config.yaml
RUN sed -i 's|directory: /var/lib/kula|directory: /app/data|' /app/config.yaml && \
    sed -i 's|listen: "127.0.0.1"|listen: "0.0.0.0"|' /app/config.yaml && \
    sed -i 's|listen: ""|listen: "0.0.0.0"|' /app/config.yaml

# Create data directory and set permissions
RUN mkdir -p /app/data && chown -R kula:kula /app

# Switch to unprivileged user
USER kula

# Expose the default port
EXPOSE 27960

# Set entrypoint
ENTRYPOINT ["/app/kula"]
CMD ["serve", "--config", "/app/config.yaml"]
