# syntax=docker/dockerfile:1
# gitweb — runtime-loaded web assets (no go:embed), so web/ must ship in the image.

FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache deps first
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO disabled for a static, portable binary
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gitweb ./cmd/gitweb

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app

WORKDIR /app
COPY --from=builder /out/gitweb /app/gitweb
COPY web /app/web

USER app
EXPOSE 8080
ENTRYPOINT ["/app/gitweb"]
CMD ["-listen", ":8080", "-base-url", "http://localhost:8080"]
