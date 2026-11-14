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
  # SSRF protection: hosts not matching allow/deny rules are blocked if private.
  allow_hosts: []
  deny_hosts: []

# Extra extensions rendered as plain text in addition to the built-in table
# (docs, code, scripts incl. .ps1/.psm1/.psd1/.bat/.cmd/.reg/.vbs). Values may
# be written with or without a leading dot; matching is case-insensitive.
# Empty (the default) behaves exactly as if this key were absent.
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
- **provider**: Single-file fetching from GitHub/GitLab/Gitea, scheme follows URL, singleflight dedup
- **cache**: In-memory LRU + TTL cache (hashicorp/golang-lru/v2)
- **render**: Markdown/HTML/text rendering — single source of truth for renderable file types
- **server**: Gin HTTP server with path routing, SSRF guard, size limits, CSP

## Security

- **No registration auth**: Public service. SSRF protection, file-size limits, and cache/timeout caps bound abuse.
- **Site password** (optional): If `password` is set (config or `-password` flag), a cookie-based login gates every page. `/login`, `/logout`, `/healthz`, and `/static/*` remain public so the login page can render.
- **SSRF protection**: `allow_hosts`/`deny_hosts` (wildcards supported); private/loopback/link-local IPs blocked by default.
- **File size**: Streamed via `io.LimitReader` — oversized files are rejected before filling memory.
- **Rate limiting**: Per-host token bucket limits external Git API calls (default 100 req/min per host, configurable via `fetch.rate_limit`). Prevents abuse and avoids hitting upstream API rate limits (e.g., GitHub's 60 req/hour for unauthenticated requests).
- **CSP + sandbox**: Viewer pages send a strict Content-Security-Policy; user `.html` is rendered in `<iframe sandbox="allow-same-origin">` (no `allow-scripts`) to stop untrusted scripts.
- **Credentials**: Only in the browser (sessionStorage), sent per-request via `Authorization`; never stored on disk, never logged, never returned by the list API.

## License

See LICENSE file.
