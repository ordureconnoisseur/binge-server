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
# python:slim (not distroless): we shell out to gallery-dl (X/Instagram)
# and yt-dlp (PornHub). Both UNPINNED — these sites rotate their private
# APIs / query-ids periodically and break older releases; a plain image
# rebuild pulls the current versions that fix it. curl_cffi gives yt-dlp
# the browser TLS impersonation PornHub demands (410s without it); ffmpeg
# is for any HLS-only PornHub download/merge.
#
# curl_cffi comes from yt-dlp's own extra rather than as a bare package.
# It is not a peer of yt-dlp but a dependency of it, and yt-dlp accepts
# only a window of versions (currently 0.5.10 and 0.10.x–0.15.x). Asking
# for it separately let pip resolve 0.16.0, which yt-dlp refused to load
# — leaving impersonation quietly switched off and every PornHub poll
# failing, on nothing worse than an image rebuild.
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir gallery-dl "yt-dlp[curl-cffi]" \
    # Fail the build here rather than ship a daemon that looks healthy and
    # cannot fetch a single video. This import is exactly what yt-dlp does
    # at runtime, and it raises when the pairing is wrong.
    && python -c "import yt_dlp.networking._curlcffi"
COPY --from=build /out/binge-server /usr/local/bin/binge-server

# Run unprivileged. The daemon shells out to yt-dlp / gallery-dl / ffmpeg
# on caller-influenced input, so a compromise of one of those must not be
# root in the container. /data is created and owned here because a mounted
# volume otherwise lands as root and the daemon could not write its DB.
RUN useradd --system --create-home --uid 10001 binge     && mkdir -p /data && chown binge:binge /data
USER binge

# Persistent data (SQLite + the generated gallery-dl cookie config).
# Mount a volume here.
VOLUME ["/data"]
ENV BINGE_DB_PATH=/data/binge-server.db

# Default listen addr — overridable. 0.0.0.0 is required because the
# bypass container shares the network namespace; binding to 127.0.0.1
# would not be reachable from other namespaces or the host port-forward.
ENV BINGE_LISTEN_ADDR=0.0.0.0:7878
EXPOSE 7878

# Lets compose/orchestrators tell "container is up" from "daemon is
# actually serving" — the two diverge on a bad mount or a port clash.
# /healthz is unauthenticated and cheap (a few COUNT(*)s). python is
# already in this image for gallery-dl, so no extra install for curl.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3     CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:7878/healthz', timeout=4).status==200 else 1)"

ENTRYPOINT ["binge-server"]
