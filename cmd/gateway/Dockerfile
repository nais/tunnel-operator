FROM golang:1.26 AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gateway ./cmd/gateway/

FROM alpine:3.21
RUN apk add --no-cache tcpdump libcap && \
    setcap cap_net_raw=eip /usr/bin/tcpdump && \
    apk del libcap
COPY --from=builder /build/gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
