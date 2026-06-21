# gitweb

Transform git repositories into browsable web pages.

## Features

- **Path-based routing**: Each repository gets a unique path ID (custom or auto-generated)
- **On-demand fetching**: Fetches single files from remote git repositories (GitHub/GitLab/Gitea)
- **Multiple formats**: Renders `.html`, `.md`, `.txt` files with automatic pagination
- **Private repository support**: Token and basic authentication
- **In-memory caching**: TTL-based cache with LRU eviction
- **Responsive UI**: Clean, minimal design with dark mode and bilingual support (EN/中文)
- **Runtime registration**: Add repositories via HTTP API while running

## Quick Start

```bash
# Build
go build -o gitweb ./cmd/gitweb

# Run with defaults
./gitweb

# Run with config file
./gitweb --config config.yaml

# Run with CLI flags
./gitweb --base-url http://example.com --admin-token secret123
```

Visit `http://localhost:8080` to register repositories.

## Configuration

See `config.example.yaml` for a full example.

```yaml
base_url: http://localhost:8080
listen: ":8080"
admin_token: "your-secret-token"  # Protect registration API

cache:
  ttl: 60s
  max_entries: 2048
  max_file_size: 5242880  # 5MB

fetch:
  timeout: 10s
  https_only: false

sites:
  - git_url: https://github.com/username/repo
    pathid: mysite
    ref: main
    auth:
      type: token
      token: ${GITHUB_TOKEN}
```

## API

### Register a site
```bash
curl -X POST http://localhost:8080/api/sites \
  -H "Authorization: Bearer your-token" \
  -H "Content-Type: application/json" \
  -d '{
    "git_url": "https://github.com/username/repo",
    "pathid": "mysite",
    "ref": "main"
  }'
```

### List sites
```bash
curl http://localhost:8080/api/sites \
  -H "Authorization: Bearer your-token"
```

### Delete a site
```bash
curl -X DELETE http://localhost:8080/api/sites/mysite \
  -H "Authorization: Bearer your-token"
```

### Refresh cache
```bash
curl -X POST http://localhost:8080/api/sites/mysite/refresh \
  -H "Authorization: Bearer your-token"
```

## Usage

After registering a repository with pathid `mysite`:

- `http://localhost:8080/mysite/` → renders `index.html`
- `http://localhost:8080/mysite/docs/guide.md` → renders `docs/guide.md`
- `http://localhost:8080/mysite/data.txt` → renders `data.txt`

Long files are automatically paginated with `?page=N`.

## Architecture

- **config**: Configuration loading (YAML + env vars)
- **registry**: Site registration and lookup
- **provider**: Single-file fetching from GitHub/GitLab/Gitea
- **cache**: In-memory TTL+LRU cache
- **render**: Markdown/HTML/text rendering with pagination
- **server**: Gin HTTP server with path routing

## Security

- **Admin token**: Protect registration API with `admin_token`
- **SSRF protection**: Configure `allow_hosts`/`deny_hosts` to prevent internal network access
- **Content isolation**: All sites share the same origin; use CSP headers for untrusted content
- **No credential logging**: Private auth tokens are not exposed in logs or list API

## License

See LICENSE file.
