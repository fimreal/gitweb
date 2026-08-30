# syntax=docker/dockerfile:1
# gitweb — web assets are embedded into the Go binary via //go:embed,
# so the final image only needs the single static binary.

FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache deps first
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO disabled for a static, portable binary; web/ is embedded at build time.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gitweb ./cmd/gitweb

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app

WORKDIR /app
COPY --from=builder /out/gitweb /app/gitweb

# /app 属于 root，而进程跑在 uid 10001 —— state 若仍用默认的 ./gitweb.state.json
# 必然写不进去，且容器现在启动即校验可写性、失败就拒绝启动。故给一个可写目录。
RUN mkdir -p /data && chown app:app /data
ENV GITWEB_STATE=/data/gitweb.state.json

USER app
EXPOSE 8080
ENTRYPOINT ["/app/gitweb"]
CMD ["-listen", ":8080", "-base-url", "http://localhost:8080"]
