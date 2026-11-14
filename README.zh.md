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
- **内存缓存**：LRU + TTL 缓存；重启后清空已拉取的文件内容
- **站点注册表持久化**：运行期添加的站点通过 JSON state 文件跨重启保留（配置项 `state_file`，可用 `-state` / `GITWEB_STATE` 覆盖）。文件只存路由数据 —— 路径 ID、git URL、ref、平台、浏览数、hidden 标记，绝不存凭据。详见下文〈站点 state 文件〉
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

# 把站点注册表持久化到指定路径
# （默认 ./gitweb.state.json，相对的是进程当前工作目录）
./gitweb --state /var/lib/gitweb/gitweb.state.json
```

### 使用 Docker 运行

```bash
# 使用预构建镜像
# 命名卷保住站点注册表（/data/gitweb.state.json），重建容器后依然在。
# 不挂这个卷的话，运行期添加的站点会随容器重建丢失。
docker run -d -p 8080:8080 \
  -v $(pwd)/config.yaml:/app/config.yaml:ro \
  -v gitweb-state:/data \
  --name gitweb \
  ${DOCKERHUB_USERNAME}/gitweb:latest \
  /app/gitweb --config /app/config.yaml

# 或本地构建
docker build -t gitweb .
docker run -d -p 8080:8080 -v gitweb-state:/data gitweb
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

# 站点注册表落盘位置（详见下文〈站点 state 文件〉）。优先级：显式 -state > 本行 > $GITWEB_STATE
# > ./gitweb.state.json。请给绝对路径 —— 相对路径相对的是进程当前工作目录。留空或不写这一行都
# 视为未配置，会继续向下一层继承；本行不能用来关闭持久化，要纯内存请用 -state "" 或 GITWEB_STATE=""。
# 容器部署注意：本行优先级压过镜像里的 GITWEB_STATE=/data/gitweb.state.json，用命名卷请把值写成
# /data/gitweb.state.json。
state_file: /var/lib/gitweb/gitweb.state.json  # 换成你自己的绝对路径

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

### 站点 state 文件

运行期添加的站点会落盘到一个 JSON 文件，从而跨重启保留。

- **路径**：`config.yaml` 里的 `state_file`、命令行 `-state`、环境变量 `GITWEB_STATE` 三处任选。
  `-state ""` 或 `GITWEB_STATE=""` 即以纯内存运行 —— 重启后运行期添加的站点全部丢失。
  `state_file` 取空值不属于这两种：它表示「未配置」，会继续向下一层继承。原因是 `config.yaml`
  整份会做环境变量展开，未赋值的 `${VAR}` 就是空串，把空值当「关掉」等于让一个漏设的变量静默丢站点。
- **优先级**：显式 `-state` > `state_file` > `GITWEB_STATE` > 内置默认值（`./gitweb.state.json`）。
- **存什么**：路径 ID、git URL、ref、平台、创建时间、浏览数、hidden 标记。**绝不存凭据** ——
  token 与密码只留在浏览器（sessionStorage）里。
- **与 config 中 `sites:` 的关系**：state 优先。预置站点只在其 pathid / git_url 尚未注册时才补种，
  所以改 `config.yaml` 不会重置一份已经跑起来的注册表。
- **预置凭据与重启**：state 文件不含凭据，私有预置站点重启后仍能读取。预置凭据只在首次播种时
  随站点进入内存；之后每次请求按 **pathid** 从 config 的 `sites[]` 里取 `auth` 叠加（overlay），
  不写回 state。没写 `pathid` 的预置首次播种会拿到随机 id，之后无法再匹配，私有仓库请显式写
  `pathid`；匹配不到时启动日志会打一条 warn。

两个运维上的坑：

- 默认值 `./gitweb.state.json` —— 以及 `state_file` 里写的任何相对路径 —— 都是相对**进程当前
  工作目录**解析的，既不相对二进制所在目录，也不相对 `config.yaml` 所在目录。在 systemd
  或各类守护进程下，这个目录往往不是你以为的那个，而且服务账号未必有写权限。部署时请给绝对路径；
  启动日志会打印解析后的绝对路径和有效 uid。
- 一个 state 文件只能有**一个写者**。每次落盘都会用该进程内存里的完整注册表重写整个文件，
  两个实例共用一个路径会互相抹掉对方的站点。每个实例给独立路径。

写入是原子的（同目录临时文件 + rename，权限 `0600`）。浏览数走防抖 —— 每 5 秒一次、退出前再补一次；
注册、删除、可见性变更则立即落盘。若目标目录不可写，gitweb 会在启动时直接退出，
而不是揣着一个永远写不进去的持久化继续服务。

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
- **凭据**：仅保存在浏览器（sessionStorage）中，随请求通过 `Authorization` 发送；服务端凭据仅用于当前请求，不写入缓存或持久化存储，不入日志，列表 API 也不会返回。
- **缓存隔离**：服务端内容缓存键混入本次请求所用凭据的截断 SHA-256 指纹（匿名请求使用固定标记 `anon`），因此私有仓库被带凭据抓取后，在 TTL 内不会被匿名或不同凭据的访客命中缓存读到；凭据原文既不进缓存键，也不进缓存 value。

## 许可证

见 LICENSE 文件。
