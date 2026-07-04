# gitweb

Transform git repositories into browsable web pages.

## Features

- **Path-based routing**: Each repository gets a unique path ID (custom or auto-generated)
- **On-demand fetching**: Fetches single files from remote git repositories (GitHub/GitLab/Gitea) — no clone
- **Multiple formats**: Renders `.md`/`.txt`/code as HTML, `.html` in a sandboxed iframe, images inline
- **Private repositories**: When a repo requires auth, the viewer prompts for a token or username/password at access time. Credentials stay in the browser (sessionStorage) and are sent per-request — never stored on the server
- **File tree**: Floating, draggable file browser to navigate the repo
- **In-memory caching**: LRU + TTL cache; restart clears everything (stateless)
- **Security**: SSRF protection (private/loopback hosts blocked), file-size limits via streaming, strict CSP, sandboxed HTML rendering
- **Responsive UI**: Dark mode and bilingual (EN/中文)

## Quick Start

```bash
# Build
go build -o gitweb ./cmd/gitweb

# Run with defaults
./gitweb

# Run with config file
./gitweb --config config.yaml

# Run with CLI flags
./gitweb --base-url http://example.com
```

Visit `http://localhost:8080` to register repositories.

## Configuration

See `config.example.yaml` for a full example.

```yaml
base_url: http://localhost:8080
listen: ":8080"

cache:
  ttl: 60s
  max_entries: 2048
  max_file_size: 5242880  # 5MB

fetch:
  timeout: 10s
  https_only: false
  # SSRF protection: hosts not matching allow/deny rules are blocked if private.
  # allow_hosts defaults to github.com / gitlab.com / *.gitea.* when https_only is true.
  allow_hosts: []
  deny_hosts: []

sites:
  - git_url: https://github.com/username/repo
    pathid: mysite
    ref: main
    hidden: false  # true = 不进公开站点列表，仅靠直链 /mysite/ 访问
    # auth is optional and only for preloaded private repos. For runtime-registered
    # repos, credentials are entered in the browser when the repo is opened.
    auth:
      type: token
      token: ${GITHUB_TOKEN}
```

> This is a **public service**: there is no registration auth. Anyone can register a
> repository. Rely on SSRF protection, file-size limits, and cache/timeout caps to
> bound abuse.

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
- **SSRF protection**: `allow_hosts`/`deny_hosts` (wildcards supported); private/loopback/link-local IPs blocked by default.
- **File size**: Streamed via `io.LimitReader` — oversized files are rejected before filling memory.
- **CSP + sandbox**: Viewer pages send a strict Content-Security-Policy; user `.html` is rendered in `<iframe sandbox="allow-same-origin">` (no `allow-scripts`) to stop untrusted scripts.
- **Credentials**: Only in the browser (sessionStorage), sent per-request via `Authorization`; never stored on disk, never logged, never returned by the list API.

## License

See LICENSE file.
