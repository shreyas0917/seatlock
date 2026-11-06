# Multi-stage build:
#   stage 1 ("build") has the full Go toolchain and compiles a static binary;
#   stage 2 copies ONLY that binary into a tiny Alpine image.
# The final image is small (~20 MB) and contains no compiler or source code.

# ---------- stage 1: build ----------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy go.mod/go.sum first and download modules. Docker caches this layer,
# so dependencies are only re-downloaded when go.mod or go.sum change.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 -> fully static binary (no C libraries needed at runtime).
# -ldflags="-s -w" strips debug symbols to make the binary smaller.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/api ./cmd/api

# ---------- stage 2: run ----------
FROM alpine:3.20
# Create an unprivileged user. (busybox in Alpine already provides wget,
# which the docker-compose healthcheck uses.)
RUN addgroup -S app && adduser -S -G app app
COPY --from=build /out/api /usr/local/bin/api

# Run as an unprivileged user: if the process is ever compromised,
# the attacker does not get root inside the container.
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/api"]
