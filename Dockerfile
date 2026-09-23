# Stage 1: Gateway Builder
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY main.go .
RUN go mod init bermuda-gateway && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -buildid=" -o bermuda-gateway main.go

# Stage 2: Hardened Runtime Environment
FROM alpine:3.21
WORKDIR /app

ARG XRAY_VERSION=v26.9.9

RUN apk add --no-cache ca-certificates tzdata wget unzip && \
    mkdir -p /usr/local/bin /usr/local/share/xray && \
    wget -qO /tmp/xray.zip "https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-64.zip" && \
    unzip -q /tmp/xray.zip -d /tmp/xray && \
    mv /tmp/xray/xray /usr/local/bin/xray && \
    mv /tmp/xray/geoip.dat /usr/local/share/xray/geoip.dat && \
    mv /tmp/xray/geosite.dat /usr/local/share/xray/geosite.dat && \
    chmod 0755 /usr/local/bin/xray && \
    rm -rf /tmp/xray /tmp/xray.zip && \
    apk del wget unzip && \
    adduser -D -H -u 10001 bermuda

ENV GOMEMLIMIT=800MiB \
    GOMAXPROCS=2 \
    GOGC=100 \
    GODEBUG=madvdontneed=1 \
    XRAY_LOCATION_ASSET=/usr/local/share/xray \
    TZ=UTC

COPY config.json /app/config.json
COPY --from=builder /build/bermuda-gateway /app/bermuda-gateway
RUN chown -R bermuda:bermuda /app /usr/local/bin/xray /usr/local/share/xray

USER bermuda
EXPOSE 2053
CMD ["/app/bermuda-gateway"]
