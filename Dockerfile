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
    -X github.com/ashish1099/gfetch/internal/cli.Version=${VERSION} \
    -X github.com/ashish1099/gfetch/internal/cli.Commit=${COMMIT} \
    -X github.com/ashish1099/gfetch/internal/cli.Date=${DATE}" \
    -o /usr/local/bin/gfetch ./cmd/gfetch

# Stage 2 — runtime
FROM alpine:3.23

RUN apk add --no-cache git openssh-client ca-certificates \
    && adduser -D -h /home/gfetch gfetch

COPY --from=builder /usr/local/bin/gfetch /usr/local/bin/gfetch

USER gfetch

ENTRYPOINT ["/usr/local/bin/gfetch"]
