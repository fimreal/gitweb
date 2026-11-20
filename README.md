# gitweb

English | [中文](README.zh.md)

Transform git repositories into browsable web pages.

## Features

- **Path-based routing**: Each repository gets a unique path ID (custom or auto-generated)
- **On-demand fetching**: Fetches single files from remote git repositories (GitHub/GitLab/Gitea) — no clone
- **Multiple formats**: Renders `.md`/`.txt`/code as HTML, `.html` in a sandboxed iframe, images inline
- **Private repositories**: When a repo requires auth, the viewer prompts for a token or username/password at access time. Credentials stay in the browser (sessionStorage) and are sent per-request — never stored on the server
- **Optional site password**: Set a password in config / via `-password` flag to require login before any page can be accessed. Not set = public (default).
- **File tree**: Floating, draggable file browser to navigate the repo
- **Single binary**: Web assets (templates, CSS, JS) are embedded into the Go binary via `//go:embed`. The released tarball and Docker image contain only the binary — no external `web/` directory is needed.
- **In-memory caching**: LRU + TTL cache; fetched file contents are cleared on restart
- **Site registry persistence**: Sites added at runtime survive restarts via a JSON state file (config `state_file`, overridable by `-state` / `GITWEB_STATE`). It stores routing data only — path ID, git URL, ref, provider, view count, hidden flag. Credentials are never written to disk. See [Site state file](#site-state-file).
- **Security**: SSRF protection (private/loopback hosts blocked), file-size limits via streaming, strict CSP, sandboxed HTML rendering
- **Responsive UI**: Dark mode and bilingual (EN/中文)

## Quick Start

### Build from source

```bash
# Build
go build -o gitweb ./cmd/gitweb

# Run with defaults
./gitweb

# Run with config file
./gitweb --config config.yaml

# Run with CLI flags
./gitweb --base-url http://example.com

# Require a password to access the site
./gitweb --password your-secret

# Persist the site registry to an explicit path
# (default: ./gitweb.state.json, relative to the process working directory)
./gitweb --state /var/lib/gitweb/gitweb.state.json
```

### Run with Docker

```bash
# Using the pre-built image
# The named volume keeps the site registry (/data/gitweb.state.json) across restarts.
# Without it, sites added at runtime are lost when the container is recreated.
docker run -d -p 8080:8080 \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v gitweb-state:/data \
  --name gitweb \
  ${DOCKERHUB_USERNAME}/gitweb:latest \
  /app/gitweb --config /app/config.yaml

# Or build locally
docker build -t gitweb .
docker run -d -p 8080:8080 -v gitweb-state:/data gitweb
```

Visit `http://localhost:8080` to register repositories.

### Self upgrade

```bash
gitweb upgrade
```

Downloads the latest release from the source repository's GitHub Releases and replaces the current binary. Target platforms are Linux/macOS (amd64/arm64); Windows is not supported. The update channel is HTTPS-only: every hop of an API request stays strictly on the API origin (`api.github.com`) — a cross-origin response can never become the source of release metadata; assets are downloaded through the API asset endpoint (identical behavior for public and private repos), and asset redirects are only allowed to the same origin or a fixed list of GitHub asset CDN hosts (port 443 only) — downgrades and off-list targets are rejected, with an explicit hop cap. The token is only sent to the API origin and is stripped on cross-origin redirect hops.

Before replacement the download is verified: full archive integrity (the gzip trailer checksum is always checked), executable format, architecture and header-table bounds; the GitHub API's `digest` field (sha256) is verified when present, otherwise a published `<asset>.sha256` checksum asset is verified. Be clear about what this covers: **with neither, only the gzip CRC, partial executable structure and the TLS transport are checked — there is no guarantee the file is complete and runnable**. The digest/checksum comes from the same API as the binary: it guards against transport corruption, but it is **not an independent publisher signature**. Any download or verification failure leaves the existing binary untouched.

Replacement: the new binary is written to a temp file in the same directory as the executable, then atomically renamed over it; a running process is unaffected. Restart gitweb afterwards to run the new binary.

The default source repository is public, so no token is needed. Optionally provide a GitHub access token via environment variable to raise API rate limits (required if you override the source to a private repository):

```bash
GITHUB_TOKEN=<token> gitweb upgrade
```

The source repository defaults to `fimreal/gitweb` and can be overridden:

```bash
GITWEB_UPGRADE_REPO=<owner>/<repo> gitweb upgrade
```

The command only upgrades — it never prints version information. To honor the "no version numbers in any output" rule, upgrade failures report fixed-category messages only — no paths, URLs or underlying system errors, since the install directory and download URLs may contain version numbers. The tradeoff: errors never show concrete paths.

## Configuration

See `config.example.yaml` for a full example.

```yaml
base_url: http://localhost:8080
listen: ":8080"

# Optional access password: if set, all pages require login to access.
# Leave empty / unset for public access (the default).
# password: "your-secret-password"

# Where the site registry is persisted (see "Site state file" below). Priority: explicit
# `-state` > this key > $GITWEB_STATE > ./gitweb.state.json. Give an absolute path — a
# relative one resolves against the process working directory. Empty / omitted means "not
# configured" and falls through to the next layer; it does not disable persistence. Use
# `-state ""` or `GITWEB_STATE=""` for a memory-only run. In containers this key outranks
# the image's `GITWEB_STATE=/data/gitweb.state.json` — use `/data/gitweb.state.json` to keep
# the named volume.
state_file: /var/lib/gitweb/gitweb.state.json  # your own absolute path

cache:
  ttl: 60s
  max_entries: 2048
  max_file_size: 5242880  # 5MB

fetch:
  timeout: 10s
  # GitHub Enterprise / self-hosted GitHub: set the instance root, e.g.
  # https://ghe.example.com. Repos whose git_url uses that host fetch files from
  # <base>/{owner}/{repo}/raw/{ref}/{path} and use <base>/api/v3 for tree/branches;
  # github.com repos keep using the public endpoints. Empty = public github.com.
  github_base_url: ""
  # SSRF protection: hosts not matching allow/deny rules are blocked if private.
  allow_hosts: []
  deny_hosts: []

# Extra extensions rendered as plain text in addition to the built-in table
# (docs, code, scripts incl. .ps1/.psm1/.psd1/.bat/.cmd/.reg/.vbs). Values may
# be written with or without a leading dot; matching is case-insensitive.
# Empty (the default) adds no extensions. Note that the seven Windows script
# extensions above are built in and now render as text — an intentional change
# from earlier versions, not something this key turns on or off.
# Matched files get `?raw=1` served as text/plain.
render:
  extra_text_exts: []  # e.g. [".tf", "hcl"]

sites:
  - git_url: https://github.com/username/repo
    pathid: mysite
    ref: main
    hidden: false  # true = not listed on the public site list; only accessible via direct link /mysite/
    # auth is optional and only for preloaded private repos. For runtime-registered
    # repos, credentials are entered in the browser when the repo is opened.
    auth:
      type: token
      token: ${GITHUB_TOKEN}
```

> This is a **public service**: there is no registration auth. Anyone can register a
> repository. Rely on SSRF protection, file-size limits, and cache/timeout caps to
> bound abuse.

### Site state file

Sites added at runtime are persisted to one JSON file so they survive restarts.

- **Path**: `state_file` in `config.yaml`, the `-state` flag, or `$GITWEB_STATE`. An empty
  `-state` or `GITWEB_STATE=""` runs memory-only — every runtime-registered site is then lost on
  restart. An empty `state_file` is *not* one of those: it means "not configured" and falls
  through to the next source, because `config.yaml` is expanded against the environment and an
  unset `${VAR}` would otherwise silently turn persistence off.
- **Priority**: explicit `-state` > `state_file` > `GITWEB_STATE` > built-in default
  (`./gitweb.state.json`).
- **Contents**: path ID, git URL, ref, provider, created time, view count, hidden flag.
  **Never credentials** — tokens and passwords stay in the browser (sessionStorage).
- **Relation to `sites:` in config**: the state file wins. Preset sites are seeded only when
  their path ID / git URL is not already registered, so editing `config.yaml` does not reset
  a live registry.
- **Preset credentials**: the state file never carries secrets, yet preset private repos keep
  working across restarts. Preset credentials are written into the in-memory site only at the
  first seeding; afterwards every request matches the site by **path ID** and overlays the
  `auth` block of the corresponding `sites[]` entry — state is not rewritten. A preset without
  an explicit `pathid` gets a random ID at first seeding and can no longer be matched, so give
  private presets a `pathid`; startup logs a warning when that match is missing.

Two operational traps:

- The default `./gitweb.state.json` — and any relative `state_file` — resolves against the
  **process working directory**, not the directory holding the binary and not the one holding
  `config.yaml`. Under systemd or a supervisor that is often not where you expect, and the
  service user may have no write permission there. Pass an absolute path in deployments; the
  resolved absolute path and the effective uid are printed on startup.
- A state file must have **one writer**. Each save rewrites the whole file from that process's
  in-memory registry, so two instances sharing one path erase each other's sites. Give every
  instance its own path.

Writes are atomic (temp file in the same directory + rename, mode `0600`). View counts are
debounced — flushed every 5s and once more on shutdown — while registration, deletion and
visibility changes are written immediately. If the target directory is not writable, gitweb exits
at startup instead of serving with persistence that could never succeed.

## API

No authentication. Anyone can register a repository.

### Register a site
```bash
curl -X POST http://localhost:8080/api/sites \
  -H "Content-Type: application/json" \
  -d '{
    "git_url": "https://github.com/username/repo",
    "pathid": "mysite",
    "ref": "main"
  }'
```

### List sites
```bash
curl http://localhost:8080/api/sites
```

### Delete a site
```bash
curl -X DELETE http://localhost:8080/api/sites/mysite
```

### Refresh cache
```bash
curl -X POST http://localhost:8080/api/sites/mysite/refresh
```

### File tree
```bash
curl http://localhost:8080/api/sites/mysite/tree
```

For private repos, pass credentials via the `Authorization` header (`Bearer <token>`, `token <token>`, or `Basic <base64>`); GitLab also accepts `PRIVATE-TOKEN`. The viewer does this automatically after you enter credentials in the prompt.

### Token scopes

gitweb only reads repositories — it never writes to them — so read-only tokens are enough
and should be preferred:

| Platform | Read-only scope | Write scope (only needed for pushing from elsewhere) |
| --- | --- | --- |
| GitHub | fine-grained: Contents = Read; classic: `repo` (private) / `public_repo` | Contents = Read and write; classic `repo` |
| GitLab | `read_repository` | `write_repository` |
| Gitea | `read:repository` | `write:repository` |

A read-only token fetches files normally (200), but a *write* through the same token is
rejected with **403** — Gitea spells it out as
`required=[write:repository], token scope=read:repository`. If reads work and a write
returns 403, check the token scope before suspecting the server; this is the platform
enforcing least privilege.

Gitea `ref` may be a branch, tag, or commit SHA. Gitea's raw endpoint distinguishes the
three (`/raw/branch/`, `/raw/tag/`, `/raw/commit/`) and does not resolve a SHA under
`/raw/branch/`, so gitweb probes branch → tag → commit and caches the resolved form per
repo/ref. Pinning a site to a tag or SHA therefore works, at the cost of one extra request
the first time.

## Usage

After registering a repository with pathid `mysite`:

- `http://localhost:8080/mysite/` → viewer page; auto-renders `README.md` (or `index.html`), file tree on the side
- `http://localhost:8080/mysite/docs/guide.md` → renders `docs/guide.md`
- `http://localhost:8080/mysite/data.txt` → renders `data.txt`
- `http://localhost:8080/mysite/page.html` → shows `page.html` in a sandboxed iframe

Long plain-text files are paginated client-side.

## Architecture

- **config**: Configuration loading (YAML + env-var interpolation)
- **registry**: Site registration and lookup (pathid validation, reserved-prefix check)
- **provider**: Single-file fetching from GitHub/GitLab/Gitea, scheme follows URL, singleflight dedup (scoped by credential fingerprint)
- **cache**: In-memory LRU + TTL cache (hashicorp/golang-lru/v2)
- **render**: Markdown/HTML/text rendering — single source of truth for renderable file types
- **server**: Gin HTTP server with path routing, SSRF guard, size limits, CSP
- **upgrade**: Self-update from the source repo's GitHub Releases — HTTPS-only, token confined to the API origin, verify-then-replace via atomic rename (linux/darwin)

## Security

- **No registration auth**: Public service. SSRF protection, file-size limits, and cache/timeout caps bound abuse.
- **Site password** (optional): If `password` is set (config or `-password` flag), a cookie-based login gates every page. `/login`, `/logout`, `/healthz`, and `/static/*` remain public so the login page can render.
- **SSRF protection**: `allow_hosts`/`deny_hosts` (wildcards supported); private/loopback/link-local IPs blocked by default.
- **File size**: Streamed via `io.LimitReader` — oversized files are rejected before filling memory.
- **Rate limiting**: Per-host token bucket limits external Git API calls (default 100 req/min per host, configurable via `fetch.rate_limit`). Prevents abuse and avoids hitting upstream API rate limits (e.g., GitHub's 60 req/hour for unauthenticated requests).
- **CSP + sandbox**: Viewer pages send a strict Content-Security-Policy; user `.html` is rendered in `<iframe sandbox="allow-same-origin">` (no `allow-scripts`) to stop untrusted scripts.
- **Credentials**: Only in the browser (sessionStorage), sent per-request via `Authorization`; on the server a credential is used only for the current request — never written to the cache or persistent storage, never logged, and never returned by the list API.
- **Cache isolation**: The server-side content cache is keyed by a truncated SHA-256 fingerprint of the credentials used for that request (anonymous requests use a fixed `anon` marker), so a private repo fetched with credentials is never served to an anonymous or differently-authenticated visitor from cache within the TTL. The same fingerprint scopes the provider's singleflight coalescing, so concurrent requests with different credentials never share one upstream fetch result either. Raw credentials appear neither in cache keys nor in cached values.

## License

See LICENSE file.
