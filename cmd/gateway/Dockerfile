# Build stage
FROM golang:1.26 AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gateway ./cmd/gateway/

# Final stage
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/gateway /gateway
ENTRYPOINT ["/gateway"]
