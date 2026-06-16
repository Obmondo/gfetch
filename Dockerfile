# Stage 1 — builder
FROM golang:1.26.4-alpine AS builder

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
FROM alpine:3.24.1

# uid/gid 999 is on purpose, cause puppetserver runs as 999(puppet) user
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates \
    && mkdir -p /home/gfetch \
    && chown -R 999:999 /home/gfetch

COPY --from=builder /usr/local/bin/gfetch /usr/local/bin/gfetch

USER 999:999

ENTRYPOINT ["/usr/local/bin/gfetch"]
