# gitweb

[English](README.md) | 中文

将 Git 仓库转换为可浏览的网页。

## 特性

- **基于路径的路由**：每个仓库分配一个唯一的路径 ID（可自定义或自动生成）
- **按需拉取**：从远程 Git 仓库（GitHub/GitLab/Gitea）按需获取单个文件 —— 无需完整克隆
- **多格式渲染**：`.md`/`.txt`/代码文件渲染为 HTML，`.html` 在沙箱 iframe 中展示，图片直接内联显示
- **私有仓库**：访问需要鉴权的仓库时，浏览界面会提示输入 Token 或用户名/密码。凭据保存在浏览器中（sessionStorage），随每次请求发送 —— 服务器不存储凭据
- **可选站点密码**：在配置中或通过 `-password` 参数设置密码后，访问任何页面都需要登录。不设置则默认公开访问
- **文件树**：可浮动、拖拽的文件浏览器，用于在仓库中导航
- **单二进制文件**：前端资源（模板、CSS、JS）通过 `//go:embed` 嵌入 Go 二进制。发布的压缩包和 Docker 镜像仅包含单个二进制文件，不需要额外的 `web/` 目录
- **内存缓存**：LRU + TTL 缓存；重启后清空（无状态）
- **安全**：SSRF 防护（拦截私网/回环地址）、基于流式读取的文件大小限制、严格的 CSP、沙箱化的 HTML 渲染
- **响应式 UI**：支持暗色模式和中英双语

## 快速开始

### 从源码构建

```bash
# 构建
go build -o gitweb ./cmd/gitweb

# 使用默认配置运行
./gitweb

# 使用配置文件运行
./gitweb --config config.yaml

# 使用命令行参数运行
./gitweb --base-url http://example.com

# 设置访问密码
./gitweb --password your-secret
```

### 使用 Docker 运行

```bash
# 使用预构建镜像
docker run -d -p 8080:8080 \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  --name gitweb \
  ${DOCKERHUB_USERNAME}/gitweb:latest \
  /app/gitweb --config /app/config.yaml

# 或本地构建
docker build -t gitweb .
docker run -d -p 8080:8080 gitweb
```

访问 `http://localhost:8080` 即可注册仓库。

## 配置

完整示例见 `config.example.yaml`。

```yaml
base_url: http://localhost:8080
listen: ":8080"

# 可选的访问密码：设置后所有页面都需要登录才能访问。
# 留空或不设置则默认公开访问。
# password: "your-secret-password"

cache:
  ttl: 60s
  max_entries: 2048
  max_file_size: 5242880  # 5MB

fetch:
  timeout: 10s
  # SSRF 防护：不匹配 allow/deny 规则的私网主机默认会被拦截。
  allow_hosts: []
  deny_hosts: []

sites:
  - git_url: https://github.com/username/repo
    pathid: mysite
    ref: main
    hidden: false  # true = 不进入公开站点列表，仅通过直链 /mysite/ 访问
    # auth 仅用于预置的私有仓库。运行时注册的仓库在打开时会提示在浏览器中输入凭据。
    auth:
      type: token
      token: ${GITHUB_TOKEN}
```

> 这是一个**公共服务**：没有注册鉴权，任何人都可以注册仓库。依赖 SSRF 防护、文件大小限制和缓存/超时上限来约束滥用。

## API

无需鉴权，任何人都可以注册仓库。

### 注册站点
```bash
curl -X POST http://localhost:8080/api/sites \
  -H "Content-Type: application/json" \
  -d '{
    "git_url": "https://github.com/username/repo",
    "pathid": "mysite",
    "ref": "main"
  }'
```

### 列出站点
```bash
curl http://localhost:8080/api/sites
```

### 删除站点
```bash
curl -X DELETE http://localhost:8080/api/sites/mysite
```

### 刷新缓存
```bash
curl -X POST http://localhost:8080/api/sites/mysite/refresh
```

### 文件树
```bash
curl http://localhost:8080/api/sites/mysite/tree
```

对于私有仓库，通过 `Authorization` 请求头传递凭据（`Bearer <token>`、`token <token>` 或 `Basic <base64>`）；GitLab 也支持 `PRIVATE-TOKEN`。在浏览界面输入凭据后，前端会自动处理。

## 使用说明

注册路径 ID 为 `mysite` 的仓库后：

- `http://localhost:8080/mysite/` → 浏览页面；自动渲染 `README.md`（或 `index.html`），侧边显示文件树
- `http://localhost:8080/mysite/docs/guide.md` → 渲染 `docs/guide.md`
- `http://localhost:8080/mysite/data.txt` → 渲染 `data.txt`
- `http://localhost:8080/mysite/page.html` → 在沙箱 iframe 中展示 `page.html`

长文本文件会在客户端分页展示。

## 架构

- **config**：配置加载（YAML + 环境变量插值）
- **registry**：站点注册与查询（路径 ID 校验、保留前缀检查）
- **provider**：从 GitHub/GitLab/Gitea 获取单个文件，按 URL scheme 路由，singleflight 去重
- **cache**：内存 LRU + TTL 缓存（hashicorp/golang-lru/v2）
- **render**：Markdown/HTML/文本 渲染 —— 可渲染文件类型的唯一判定来源
- **server**：Gin HTTP 服务器，包含路径路由、SSRF 防护、大小限制、CSP

## 安全

- **无注册鉴权**：公共服务。通过 SSRF 防护、文件大小限制、缓存/超时上限来约束滥用。
- **站点密码**（可选）：设置 `password`（配置文件或 `-password` 参数）后，所有页面都需要 Cookie 登录。`/login`、`/logout`、`/healthz` 和 `/static/*` 保持公开，以便登录页面正常渲染。
- **SSRF 防护**：`allow_hosts`/`deny_hosts`（支持通配符）；默认拦截私网、回环和链路本地 IP。
- **文件大小**：通过 `io.LimitReader` 流式读取，超大文件在占满内存前就会被拒绝。
- **限流**：按 host 分桶的令牌桶限流器，限制对外部 Git API 的请求（默认每个 host 100 req/min，可通过 `fetch.rate_limit` 配置）。防止滥用，并避免触及上游平台 API 限制（如 GitHub 未认证请求的 60 req/hour）。
- **CSP + 沙箱**：浏览页面下发严格的 Content-Security-Policy；用户 `.html` 在 `<iframe sandbox="allow-same-origin">`（不含 `allow-scripts`）中渲染，阻止不可信脚本执行。
- **凭据**：仅保存在浏览器（sessionStorage）中，随请求通过 `Authorization` 发送；不落盘、不入日志、列表 API 也不会返回。

## 许可证

见 LICENSE 文件。
