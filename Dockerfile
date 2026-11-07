# Multi-stage build:
#   stage "build" has the full Go toolchain and compiles static binaries;
#   the runtime stages copy ONLY one binary into a tiny Alpine image.
# Final images are small (~20 MB) and contain no compiler or source code.
#
# Targets:
#   docker build .                   -> the SeatLock API (last stage = default)
#   docker build --target payment .  -> the mock payment gateway
#   docker build --target worker .   -> the background worker

# ---------- build ----------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy go.mod/go.sum first and download modules. Docker caches this layer,
# so dependencies are only re-downloaded when go.mod or go.sum change.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 -> fully static binaries (no C libraries needed at runtime).
# -ldflags="-s -w" strips debug symbols to make the binaries smaller.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/payment ./cmd/payment \
 && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/worker ./cmd/worker

# ---------- runtime base ----------
FROM alpine:3.20 AS runtime
# Create an unprivileged user. (busybox in Alpine already provides wget,
# which the docker-compose healthchecks use.)
RUN addgroup -S app && adduser -S -G app app
# Run as an unprivileged user: if the process is ever compromised,
# the attacker does not get root inside the container.
USER app

# ---------- mock payment gateway ----------
FROM runtime AS payment
COPY --from=build /out/payment /usr/local/bin/payment
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/payment"]

# ---------- background worker ----------
FROM runtime AS worker
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["/usr/local/bin/worker"]

# ---------- API (default, because it is the last stage) ----------
FROM runtime AS api
COPY --from=build /out/api /usr/local/bin/api
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
