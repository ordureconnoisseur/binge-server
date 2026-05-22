# syntax=docker/dockerfile:1.7

# ── Build stage ────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

# Version is plumbed in via the release workflow's build-arg and
# embedded into the binary via -X main.Version. Local `docker build`
# falls back to "docker" so the binary's logs at least say where it
# came from.
ARG VERSION=docker

# Cache go module downloads layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled keeps modernc.org/sqlite in pure-Go mode (smaller image,
# no glibc dependency in the final stage).
ENV CGO_ENABLED=0
RUN go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/binge-server .

# ── Runtime stage ──────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/binge-server /binge-server

# Persistent data (SQLite). Mount a volume here.
VOLUME ["/data"]
ENV BINGE_DB_PATH=/data/binge-server.db

# Default listen addr — overridable. 0.0.0.0 is required because the
# bypass container shares the network namespace; binding to 127.0.0.1
# would not be reachable from other namespaces or the host port-forward.
ENV BINGE_LISTEN_ADDR=0.0.0.0:7878
EXPOSE 7878

USER nonroot:nonroot
ENTRYPOINT ["/binge-server"]
