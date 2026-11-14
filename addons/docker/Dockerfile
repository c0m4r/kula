# The builder runs on the build machine's platform and cross-compiles for the
# target with GOARCH below: the Go image need not exist for every platform the
# push scripts build (this pin has no linux/riscv64), and nothing in this stage
# runs under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.11-trixie@sha256:fa340cb59ebeb1d53ac1cccfc334201d938fe5f3458003cccb767096d8e032b8 AS builder

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
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

# Create the unprivileged user Kula runs as. Its ids are pinned to the ones
# `adduser -S` gave earlier images (uid 100, gid 101: gid 100 is Alpine's
# "users"), so data volumes those images wrote stay writable.
RUN addgroup -S -g 101 kula && adduser -S -u 100 -G kula kula

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

# Root owns the binary and the config, so Kula cannot rewrite either; it reads
# the config through its group. Only the data directory is Kula's.
RUN chown root:root /app /app/kula && chmod 0755 /app /app/kula && \
    chown root:kula /app/config.yaml && chmod 0640 /app/config.yaml && \
    install -d -o kula -g kula -m 0750 /app/data

# Switch to unprivileged user
USER kula

# Expose the default port
EXPOSE 27960

# Set entrypoint
ENTRYPOINT ["/app/kula"]
CMD ["serve", "--config", "/app/config.yaml"]
