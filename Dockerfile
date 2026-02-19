# Stage 1 — builder
FROM golang:1.25-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

ENV CGO_ENABLED=0

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -ldflags "-s -w \
    -X github.com/obmondo/gfetch/internal/cli.Version=${VERSION} \
    -X github.com/obmondo/gfetch/internal/cli.Commit=${COMMIT} \
    -X github.com/obmondo/gfetch/internal/cli.Date=${DATE}" \
    -o /usr/local/bin/gfetch ./cmd/gfetch

# Stage 2 — runtime
FROM debian:bookworm-slim

# uid/gid 999 is on purpose, cause puppetserver runs as 999(puppet) user
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    openssh-client \
    ca-certificates \
    && groupadd -g 999 gfetch \
    && useradd -m -u 999 -g gfetch -s /bin/bash gfetch \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/gfetch /usr/local/bin/gfetch

USER gfetch

ENTRYPOINT ["/usr/local/bin/gfetch"]
