# Multi-stage Dockerfile for ieee-office-backend
# Builder: compile the Go binary
FROM golang:1.25-alpine AS builder
WORKDIR /src

# Cache deps
COPY go.mod go.sum* ./
RUN if [ -f go.mod ]; then go mod download; fi

# Copy source and static files needed at runtime
COPY . .

# Build a small, stripped linux binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /attendance ./

# Final: minimal runtime image
FROM alpine:latest

RUN apk add --no-cache tzdata ca-certificates

WORKDIR /

# Copy binary
COPY --from=builder /attendance /attendance

EXPOSE 8080

ENTRYPOINT ["/attendance"]
