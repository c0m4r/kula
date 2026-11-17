# kula-scan for ./showcase.sh test, built from this checkout as
# kula-scan:showcase. An image of its own: the scanner never enters Kula's.
# Build context: the repository root, as for ../../Dockerfile.
FROM --platform=$BUILDPLATFORM golang:1.26.11-trixie@sha256:fa340cb59ebeb1d53ac1cccfc334201d938fe5f3458003cccb767096d8e032b8 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -buildvcs=false -o /kula-scan ./cmd/kula-scan/

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

COPY --from=builder /kula-scan /usr/local/bin/kula-scan

USER 65534:65534

ENTRYPOINT ["/usr/local/bin/kula-scan"]
