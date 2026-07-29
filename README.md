# binge-server

A small Go daemon that fetches performers' social media on behalf of the [binge Stash plugin](https://github.com/ordureconnoisseur/binge), so their latest posts show up in your stories row alongside library scenes and StashDB releases.

It backs four features in binge:

| Pillar | What it adds |
|-|-|
| **Reddit** | New posts from performers' Reddit accounts in the stories row |
| **X (Twitter)** | Recent X media folded into a performer's profile story |
| **PornHub** | New uploads in the stories row and on the performer's scene grid, streamed on demand |
| **Save to Stash** | Downloads a post's media, files it into your library, and tags it |

**Optional.** binge works fine without binge-server — each pillar silently no-ops when the daemon is unreachable. The library reel, Home feed, performer profiles, StashDB discovery and collections do not touch it. Install this only if you want the four features above.

---

## Why a separate daemon?

Reddit killed self-service OAuth signups in November 2025, so the only way to authenticate against their JSON endpoints now is with a browser-derived session cookie. That cookie isn't something a browser-side plugin can use directly — different origin, no way for a SPA to send `Cookie: reddit_session=…` to reddit.com.

binge-server keeps the cookie on the same machine as Stash, polls Reddit on a fixed interval (default 4h), classifies posts by type (image / video / text / link), resolves redgifs videos to direct mp4 URLs, and exposes a small HTTP API the binge plugin consumes. It also proxies image/video CDN requests to sidestep hotlink-protection 403s.

The same reasoning covers the other pillars: X needs session cookies a browser plugin can't send cross-origin, PornHub needs server-side extraction, and Save has to write files into your Stash library. All three shell out to `gallery-dl` / `yt-dlp` / `ffmpeg`, which a browser obviously cannot.

## Install

### Option 1 — Docker Compose (recommended)

Grab [`docker-compose.yml`](./docker-compose.yml) and:

```bash
docker compose up -d
```

That's the whole install. The defaults cover the common case (Stash on the same machine); the comments in the file flag the two things worth a look, the port bind and the Save-to-Stash paths.

The container ships a real healthcheck, so `docker ps` distinguishes "running" from "actually serving":

```bash
docker compose ps                      # STATUS shows (healthy)
curl -s localhost:7878/healthz | jq    # version, config state, counts
```

<details>
<summary>Or plain <code>docker run</code></summary>

```bash
docker run -d \
  --name binge-server \
  --restart unless-stopped \
  -p 127.0.0.1:7878:7878 \
  -v ~/binge-server-data:/data \
  ghcr.io/ordureconnoisseur/binge-server:latest
```

`BINGE_DB_PATH` already defaults to `/data/binge-server.db` in the image, so it only needs setting if you want the DB elsewhere.

</details>

The bind `127.0.0.1:7878` keeps the daemon reachable only from the same machine. Drop the `127.0.0.1:` prefix if you need to expose it to your LAN or tailnet — but not to the internet, since the daemon holds your Stash API key.

**CORS / credentials:** the daemon protects its credential-writing endpoints against cross-origin browser attacks. Stash served from **localhost, a LAN IP, or a Tailscale host is allowed automatically** — no config needed. You only need to set `BINGE_ALLOWED_ORIGIN` (to your Stash origin, e.g. `https://stash.example.com`) when Stash is served from a **public domain** behind a reverse proxy.

Once it's up, open binge → Settings → "binge-server configuration" card and paste your Reddit session cookie there. (Stash API key is auto-detected.)

**External tools:** the Docker image bundles `gallery-dl`, `yt-dlp`, `ffmpeg` and `curl_cffi`, so nothing extra is needed. The binary and source installs below do **not** — Reddit works without them, but X, PornHub and Save each shell out to those tools and will no-op until they're on `PATH`.

### Option 2 — Pre-built binary

Each tagged release on [GitHub Releases](https://github.com/ordureconnoisseur/binge-server/releases) ships binaries for:

- `binge-server_vX.Y.Z_darwin_arm64.tar.gz` — Apple Silicon Macs
- `binge-server_vX.Y.Z_darwin_amd64.tar.gz` — Intel Macs
- `binge-server_vX.Y.Z_linux_amd64.tar.gz`
- `binge-server_vX.Y.Z_linux_arm64.tar.gz`
- `binge-server_vX.Y.Z_windows_amd64.zip`

Unpack and run:

```bash
tar -xzf binge-server_v0.2.0_linux_amd64.tar.gz
cd binge-server_v0.2.0_linux_amd64
./binge-server
```

The Docker image is also published to GHCR alongside each release:

```bash
docker pull ghcr.io/ordureconnoisseur/binge-server:latest
docker pull ghcr.io/ordureconnoisseur/binge-server:v0.2.0
```

### Option 3 — Build from source

```bash
git clone https://github.com/ordureconnoisseur/binge-server.git
cd binge-server
go build .
./binge-server
```

Requires Go 1.22+. SQLite is embedded via `modernc.org/sqlite` — no CGO needed. For the X / PornHub / Save pillars, also install `gallery-dl`, `yt-dlp` and `ffmpeg` and keep them on `PATH` (leave them unpinned: these sites rotate their private APIs, so a current version is usually the fix when extraction breaks).

## Configuration

Credentials, all set from binge → Settings → "binge-server configuration":

1. **Stash API key** — auto-detected from Stash's `configuration.general.apiKey` query. Binge fills this in for you when the configuration card first loads.
2. **Reddit session cookie** — has to be pasted manually because cookies live in a different browser origin than Stash. Expand "How to find your Reddit cookie" for the four-step instructions. Cookies expire every few months; repeat when stories stop updating.
3. **X cookies** (`auth_token` + `ct0`) — only for the X pillar, and only together: `auth_token` is useless without `ct0`. Same rotation caveat as Reddit.

PornHub needs no credentials. Save additionally needs a write path — see `BINGE_SOCIAL_WRITE_ROOT` below.

These are stored in SQLite (`binge-server.db`, in `/data` if you mounted the Docker volume). Updating a cookie via the binge UI takes effect on the next poll cycle (4h by default, or trigger a manual `POST /reddit/refresh`).

### Optional environment variables

| Variable | Default | What it does |
|-|-|-|
| `BINGE_LISTEN_ADDR` | `127.0.0.1:7878` | Address to bind |
| `BINGE_DB_PATH` | `binge-server.db` | SQLite file location |
| `BINGE_ALLOWED_ORIGIN` | _(unset)_ | Extra CORS origins. Loopback/private/tailnet are auto-allowed; set this only for a **public** Stash origin (e.g. `https://stash.example.com`), comma-separated. `*` is ignored. |
| `BINGE_POLL_INTERVAL` | `4h` | How often to poll Reddit |
| `BINGE_PORNHUB_POLL_INTERVAL` | `12h` | How often to poll PornHub (heavier than Reddit — yt-dlp per performer) |
| `BINGE_SOCIAL_WRITE_ROOT` | _(unset)_ | Where `POST /save` writes downloaded media. Save is disabled until this is set |
| `BINGE_SOCIAL_STASH_ROOT` | _(unset)_ | The same location as Stash sees it, when the daemon's path differs (e.g. in Docker) |
| `BINGE_PERFORMER_SYNC_INTERVAL` | `24h` | How often to re-scan Stash for new performer Reddit URLs |
| `STASH_URL` | `http://localhost:9999` | Initial-seed-only — overrideable via UI |
| `STASH_API_KEY` | (empty) | Initial seed — auto-detected from Stash same-origin in normal use |
| `REDDIT_SESSION_COOKIE` | (empty) | Initial seed — paste via UI in normal use |
| `X_AUTH_TOKEN` | (empty) | Initial seed — paste via UI in normal use |
| `X_CT0` | (empty) | Initial seed — must accompany `X_AUTH_TOKEN` |
| `REDDIT_USER_AGENT` | `binge-server/0.2` | Identifier sent to Reddit (their ToS requires a distinctive UA) |

The env vars are *initial seeds* — they populate the live config store only if the corresponding key is unset. Once you've configured via the UI, the env vars stop mattering.

## How performer discovery works

binge-server reads every performer's `urls` field from your Stash library. URLs matching either of these patterns mark the performer as "Reddit-polled":

- `reddit.com/user/<handle>` or `reddit.com/u/<handle>` → polls that user's submissions
- `reddit.com/r/<sub>` → polls the new feed of that subreddit

If a performer has both, the user feed wins. Performers without a Reddit URL are silently skipped.

The other pillars key off the same `urls` field:

- `x.com/<handle>` or `twitter.com/<handle>` → X media on that performer's profile story
- `pornhub.com/...` → PornHub uploads in stories and on the scene grid

A performer with none of these is simply not polled — there's no cost to leaving them unset.

The full re-scan happens every 24 hours by default. Add a Reddit URL to a performer in Stash, then trigger a manual sync with `curl -X POST localhost:7878/reddit/refresh` to pick it up immediately.

## HTTP API

| Method | Path | Description |
|-|-|-|
| GET | `/healthz` | `{ ok, version, configured, lastPerformerSync, lastPoll, performerCount, postCount }`. Unauthenticated, and what the container healthcheck polls — `version` tells you which build is actually running |
| GET | `/config` | Public shape of stored config — booleans for which secrets are set, never the values |
| POST | `/config` | Body: `{stashUrl?, stashApiKey?, redditSessionCookie?}`. Validates each non-empty field against the live service before persisting |
| GET | `/reddit/stories?sinceUtc=N` | Per-performer digest, used by binge's stories row |
| GET | `/reddit/feed/{stashId}?limit=25` | Paginated posts for one performer |
| POST | `/reddit/refresh` | Trigger a poll cycle (debounced 30s) |
| GET | `/redgifs/proxy?url=...` | Hotlink-evading proxy for redgifs CDN videos |
| GET | `/reddit/proxy?url=...` | Same for `*.redd.it` and `*.redditmedia.com` images |
| GET | `/x/feed/{stashId}?days=7` | Recent X media for one performer, fetched on demand via gallery-dl |
| GET | `/x/handle/{handle}` | Resolve an X handle without a Stash performer |
| GET | `/pornhub/stories` | Per-performer digest of new uploads, for the stories row |
| GET | `/pornhub/feed/{stashId}` | Cached video list for one performer |
| GET | `/pornhub/stream/{videoId}` | mp4 stream proxy (iOS AVPlayer can't decode the webm previews) |
| GET | `/pornhub/preview/{videoId}` | Hover-preview (mediabook) proxy |
| GET | `/pornhub/thumb?url=...` | Thumbnail proxy |
| POST | `/save` | Download a post's media, place it in your library, and tag it |

Daemon binds `127.0.0.1` by default → the proxies and `/config` are only reachable from the same machine.

## Architecture notes

- **SQLite, file-backed.** No external services. `binge-server.db` holds performers, posts, sync state, and live config.
- **Live config.** Stash URL + API key + Reddit cookie are stored in the DB (not env vars) so the binge UI can rotate the cookie without restarting the daemon. Reads use an in-memory cache with an RWMutex.
- **Retention.** Posts older than 90 days are swept nightly so the DB stays bounded regardless of how many performers you follow.
- **Rate limiting.** 100ms sleep between Reddit requests, well under the 600 req / 10 min budget. The poller bails on `429 Too Many Requests` and waits for the next tick.
- **NSFW-by-default.** binge is built for NSFW content; no extra filtering layer. If you want a SFW build, fork and remove the redgifs resolver.

## Development

```bash
go run .
```

Then in another terminal:

```bash
curl localhost:7878/healthz | jq
```

The Reddit + Stash clients accept runtime credential swaps via `SetCookie` / `SetCredentials`, so the poller re-reads from the config store on every tick — no daemon restart needed when the cookie rotates.

## License

AGPL-3.0. See [LICENSE](./LICENSE). (Matches Stash's own license.)
