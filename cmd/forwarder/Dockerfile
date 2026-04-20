FROM golang:1.26 AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o forwarder ./cmd/forwarder/

FROM scratch
COPY --from=builder /build/forwarder /forwarder
USER 65532:65532
ENTRYPOINT ["/forwarder"]
