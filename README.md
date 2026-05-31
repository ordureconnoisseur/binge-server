# binge-server

A small Go daemon that polls Reddit on behalf of the [binge Stash plugin](https://github.com/ordureconnoisseur/binge), so new posts from performers' Reddit accounts show up in your stories row alongside library scenes and StashDB releases.

**Optional.** binge works fine without binge-server — the Reddit pillar just silently no-ops. Install this only if you want Reddit posts in stories.

---

## Why a separate daemon?

Reddit killed self-service OAuth signups in November 2025, so the only way to authenticate against their JSON endpoints now is with a browser-derived session cookie. That cookie isn't something a browser-side plugin can use directly — different origin, no way for a SPA to send `Cookie: reddit_session=…` to reddit.com.

binge-server keeps the cookie on the same machine as Stash, polls Reddit on a fixed interval (default 4h), classifies posts by type (image / video / text / link), resolves redgifs videos to direct mp4 URLs, and exposes a small HTTP API the binge plugin consumes. It also proxies image/video CDN requests to sidestep hotlink-protection 403s.

## Install

### Option 1 — Docker (recommended)

```bash
docker run -d \
  --name binge-server \
  --restart unless-stopped \
  -p 127.0.0.1:7878:7878 \
  -v ~/binge-server-data:/data \
  -e BINGE_DB_PATH=/data/binge-server.db \
  ghcr.io/ordureconnoisseur/binge-server:latest
```

The bind `127.0.0.1:7878` keeps the daemon reachable only from the same machine. Drop the `127.0.0.1:` prefix if you need to expose it to your LAN.

**CORS / credentials:** the daemon protects its credential-writing endpoints against cross-origin browser attacks. Stash served from **localhost, a LAN IP, or a Tailscale host is allowed automatically** — no config needed. You only need to set `BINGE_ALLOWED_ORIGIN` (to your Stash origin, e.g. `https://stash.example.com`) when Stash is served from a **public domain** behind a reverse proxy.

Once it's up, open binge → Settings → "binge-server configuration" card and paste your Reddit session cookie there. (Stash API key is auto-detected.)

### Option 2 — Pre-built binary

Each tagged release on [GitHub Releases](https://github.com/ordureconnoisseur/binge-server/releases) ships binaries for:

- `binge-server_vX.Y.Z_darwin_arm64.tar.gz` — Apple Silicon Macs
- `binge-server_vX.Y.Z_darwin_amd64.tar.gz` — Intel Macs
- `binge-server_vX.Y.Z_linux_amd64.tar.gz`
- `binge-server_vX.Y.Z_linux_arm64.tar.gz`
- `binge-server_vX.Y.Z_windows_amd64.zip`

Unpack and run:

```bash
tar -xzf binge-server_v0.1.0_linux_amd64.tar.gz
cd binge-server_v0.1.0_linux_amd64
./binge-server
```

The Docker image is also published to GHCR alongside each release:

```bash
docker pull ghcr.io/ordureconnoisseur/binge-server:latest
docker pull ghcr.io/ordureconnoisseur/binge-server:v0.1.0
```

### Option 3 — Build from source

```bash
git clone https://github.com/ordureconnoisseur/binge-server.git
cd binge-server
go build .
./binge-server
```

Requires Go 1.22+. SQLite is embedded via `modernc.org/sqlite` — no CGO needed.

## Configuration

Two credentials are needed for Reddit polling to work:

1. **Stash API key** — auto-detected from Stash's `configuration.general.apiKey` query. Binge fills this in for you when the binge-server configuration card first loads.
2. **Reddit session cookie** — has to be pasted manually because cookies live in a different browser origin than Stash. Open binge → Settings → "binge-server configuration" → expand "How to find your Reddit cookie" for the four-step instructions. Cookies expire every few months; repeat when stories stop updating.

Both are stored in SQLite (`binge-server.db`, in `/data` if you mounted the Docker volume). Updating the cookie via the binge UI takes effect on the next poll cycle (4h by default, or trigger a manual `POST /reddit/refresh`).

### Optional environment variables

| Variable | Default | What it does |
|-|-|-|
| `BINGE_LISTEN_ADDR` | `127.0.0.1:7878` | Address to bind |
| `BINGE_DB_PATH` | `binge-server.db` | SQLite file location |
| `BINGE_ALLOWED_ORIGIN` | _(unset)_ | Extra CORS origins. Loopback/private/tailnet are auto-allowed; set this only for a **public** Stash origin (e.g. `https://stash.example.com`), comma-separated. `*` is ignored. |
| `BINGE_POLL_INTERVAL` | `4h` | How often to poll Reddit |
| `BINGE_PERFORMER_SYNC_INTERVAL` | `24h` | How often to re-scan Stash for new performer Reddit URLs |
| `STASH_URL` | `http://localhost:9999` | Initial-seed-only — overrideable via UI |
| `STASH_API_KEY` | (empty) | Initial seed — auto-detected from Stash same-origin in normal use |
| `REDDIT_SESSION_COOKIE` | (empty) | Initial seed — paste via UI in normal use |
| `REDDIT_USER_AGENT` | `binge-server/0.2` | Identifier sent to Reddit (their ToS requires a distinctive UA) |

The env vars are *initial seeds* — they populate the live config store only if the corresponding key is unset. Once you've configured via the UI, the env vars stop mattering.

## How performer discovery works

binge-server reads every performer's `urls` field from your Stash library. URLs matching either of these patterns mark the performer as "Reddit-polled":

- `reddit.com/user/<handle>` or `reddit.com/u/<handle>` → polls that user's submissions
- `reddit.com/r/<sub>` → polls the new feed of that subreddit

If a performer has both, the user feed wins. Performers without a Reddit URL are silently skipped.

The full re-scan happens every 24 hours by default. Add a Reddit URL to a performer in Stash, then trigger a manual sync with `curl -X POST localhost:7878/reddit/refresh` to pick it up immediately.

## HTTP API

| Method | Path | Description |
|-|-|-|
| GET | `/healthz` | `{ ok, configured, lastPerformerSync, lastPoll, performerCount, postCount }` |
| GET | `/config` | Public shape of stored config — booleans for which secrets are set, never the values |
| POST | `/config` | Body: `{stashUrl?, stashApiKey?, redditSessionCookie?}`. Validates each non-empty field against the live service before persisting |
| GET | `/reddit/stories?sinceUtc=N` | Per-performer digest, used by binge's stories row |
| GET | `/reddit/feed/{stashId}?limit=25` | Paginated posts for one performer |
| POST | `/reddit/refresh` | Trigger a poll cycle (debounced 30s) |
| GET | `/redgifs/proxy?url=...` | Hotlink-evading proxy for redgifs CDN videos |
| GET | `/reddit/proxy?url=...` | Same for `*.redd.it` and `*.redditmedia.com` images |

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
