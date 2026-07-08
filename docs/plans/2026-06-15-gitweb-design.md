# gitweb 设计文档

## 1. 概述

gitweb 是一个用 Go + Gin 实现的轻量服务：给定任意 git 仓库地址，
服务为其分配一个短标识 `pathid`（自定义或随机 8 位），访问
`BASE_URL/<pathid>/<文件路径>` 时，服务按需从远端拉取**单个文件**内容，
把文本文件（`.html` / `.txt` / `.md` 等）实时渲染为网页返回。

核心心智模型：

```
注册：  https://github.com/username/helloworld  ──►  pathid = ab12cd34（或自定义 mytest）
访问：  BASE_URL/ab12cd34/            ──►  拉取并渲染仓库内 index.html
        BASE_URL/ab12cd34/docs/a.md  ──►  拉取并渲染 docs/a.md
```

关键设计取舍（依据最新需求）：

- **不在本地 clone 仓库**：每次请求按文件路径，通过拼接 raw 地址或调用平台
  API 获取**单个文件**，不落地整个仓库。
- **用 path 前缀而非子域名**识别站点：注册得到 `pathid`，访问走 `BASE_URL/<pathid>/...`。
- **不处理文件间跳转关系**：不重写相对链接 / 资源路径，只负责「能访问到并渲染单个文件」。
- 服务进程**可持久化站点注册表与浏览数**：站点映射与文件缓存在内存中，
  但站点注册表（pathid → git_url 等）与浏览数可通过 state 文件落盘，重启恢复；
  文件缓存仍为内存级，重启即失（详见 §4.1）。
- **可选全局访问密码**：部署方可配置 `password`，设置后所有页面需登录会话才能访问
  （cookie 会话 + Secure 标志，详见 §8）。未设置时为完全公开服务。

站点来源两类：

- **启动时预置**：命令行参数或配置文件指定一批 `git_url → pathid`。
- **运行时动态注册**：通过 HTTP API 在运行期添加新站点。

## 2. 目标与非目标

### 目标
- 把任意 git 仓库（公开 / 私有）的**单个文本文件**实时转化为可浏览网页。
- 支持主流托管平台：GitHub / GitLab / Gitea（含自建实例，含自建 http 内网实例）。
- 私有仓库支持 token 与账号密码鉴权（用于 raw / API 请求头）。
- path 前缀路由：每个仓库绑定一个指定或随机 8 位 `pathid`。
- 内存缓存单文件内容与渲染结果，按 TTL 失效。
- 渲染 `.html`（原样）、`.txt` / `.md`（转为 HTML），长内容前端分页。
- 首页极简风、响应式、中英文双语、暗黑模式自动/手动切换。
- 站点注册表与浏览数可持久化到 state 文件，重启恢复；文件缓存仅内存。
- 站点浏览数统计：每次打开 viewer 页面累加，防抖落盘。
- 站点可见性开关：可标记隐藏，不进公开列表但直链仍可访问。
- 按远端 host 限流，避免对单一 Git 平台过度请求被对方封禁。
- 可选全局访问密码：部署方可锁站，登录会话才可访问。
- 支持 HTTP/HTTPS 代理出网，便于内网部署访问公网 Git 平台。
- 优雅退出：收到 SIGINT/SIGTERM 后 flush 状态再退出。

### 非目标（YAGNI）
- 不 clone / pull 整个仓库，不维护本地工作区。
- 不重写文件内的相对链接、`<img>`/`<script>`/`<a>` 等资源路径（不解决跳转）。
- 不做用户账户体系 / 权限管理（仅有可选的全局访问密码，单角色）。
- 不做持久化数据库（Redis 等留作未来扩展）。
- 不做 git 写操作；只读。
- 不做 commit 历史 / diff / blame 浏览。
- 不做"不识别 host 的通用 raw 模板回退"：未识别 host 直接报错，避免盲目请求任意 host（原通用回退已移除，见 §4.2）。

## 3. 架构

### 3.1 分层

```
                         ┌─────────────────────────────────┐
   浏览器 / HTTP 请求  ─► │            Gin HTTP 层            │
                         │  - path 路由 /:pathid/*filepath  │
                         │  - 首页 / 注册 API / 静态资源     │
                         └───────────────┬─────────────────┘
                                         │
                         ┌───────────────▼─────────────────┐
                         │           Registry              │
                         │  pathid ──► Site(git_url, auth) │
                         │  内存 map + RWMutex             │
                         └───────────────┬─────────────────┘
                                         │
                ┌────────────────────────┼────────────────────────┐
                │                        │                        │
       ┌────────▼────────┐     ┌─────────▼────────┐     ┌─────────▼────────┐
       │   Provider      │     │   Cache          │     │   Renderer       │
       │  解析 git_url → │     │  内存 LRU + TTL  │     │  md/txt/html →   │
       │  raw URL / API  │     │  文件 & 渲染结果 │     │  HTML + 分页     │
       │  取单文件字节   │     │                  │     │                  │
       └─────────────────┘     └──────────────────┘     └──────────────────┘
                │
        ┌───────┴────────┬─────────────┐
   GitHub Provider  GitLab Provider  Gitea Provider   (按 host 识别)
```

### 3.2 请求生命周期（访问站点）

1. 请求 `GET BASE_URL/ab12cd34/docs/a.md` 进入 Gin。
2. （若启用全局密码）`authMiddleware` 校验登录会话 cookie，未登录 → 302 到 `/login`。
3. 路由 `/:pathid/*filepath` 解析出 `pathid = ab12cd34`、`filepath = docs/a.md`。
4. 用 `pathid` 查 `Registry`，拿到 `Site`（git_url、ref、鉴权、views、hidden）。查不到 → 404。
5. `filepath` 为空或 `/` → 这是 viewer 页面访问：`Views` 累加 1（置 dirty，由后台
   flush goroutine 防抖落盘），返回 viewer 骨架；首屏由前端请求 README/index。
6. `filepath` 非空 → 向 `Cache` 查 `(pathid, filepath)` 渲染结果：命中且未过期 → 直接返回。
7. 未命中 → `Provider` 先过 host 限流，再把 `(git_url, ref, filepath)` 解析成 raw URL
   或平台 API 请求，带上鉴权头，HTTP 拉取**单个文件**字节。
8. `Renderer` 按扩展名渲染：`.html` 原样、`.md`/`.txt` 转 HTML；长内容前端分页。
9. 结果写入 `Cache`，返回浏览器。

### 3.3 path 路由方案

- 配置 `BASE_PATH`（一般为根 `/`）。路由形如 `/:pathid/*filepath`。
- 保留前缀避免冲突：`/api/...`、`/static/...`、`/healthz`、根 `/`（首页）
  优先匹配，不被当作 `pathid`。
- `pathid`：自定义（校验字符集 `[a-zA-Z0-9_-]`、长度上限、唯一）或随机 8 位
  base32 串（去除易混字符）。
- 相比子域名方案，无需泛域名解析与通配证书，单证书 / 单域即可，部署更简单。

### 3.4 优雅退出

- 进程监听 SIGINT/SIGTERM；收到信号后：
  1. `http.Server.Shutdown(ctx)` 停止接受新连接、处理完在途请求（3 秒超时）。
  2. `Registry.Close()` 触发最后一次 flush，把内存中 dirty 的浏览数与注册表
     写入 state 文件，避免重启丢失。
- 启动时若指定 state 文件且存在，则先加载已持久化的站点与浏览数，再合并配置
  中的预置站点（预置站点若 pathid 已存在则跳过，避免重复种入）。

## 4. 核心模块

### 4.1 Registry（站点注册表）
- 数据结构：`map[string]*Site`，键为 `pathid`，配 `sync.RWMutex`。
- `Site` 字段：`PathID`、`GitURL`、`Ref`（分支/标签/commit，默认 `main`）、
  `Auth`（类型 + 凭据，仅预置私有仓库用；运行时输入的凭据不落表）、
  `Provider`（github/gitlab/gitea，按 host 推断）、`CreatedAt`、
  `Views int64`（累计打开次数）、`Hidden bool`（隐藏开关，仅创建时可设）。
  （设计原写的 `DefaultDoc` 字段已删除——入口文件由 viewer 前端按
  README.md → index.html → README.txt 顺序探测，不在注册表固化。）
- 操作：`Register`（pathid 冲突检测 + 保留前缀校验；未指定则随机生成 8 位）、
  `Get`、`ListPublic`（仅返回 `Hidden=false` 的站点，用于公开 `/api/sites`）、
  `Remove`、`SetHidden`（运行时切换可见性）、`IncrementViews`（浏览数 +1 并置 dirty）、
  `HasGitURL`（预置去重用）。
- **持久化**（可选）：`EnablePersistence(path)` 加载 state 文件并启动后台 flush
  goroutine，每 5 秒把 dirty 的浏览数与注册表增量落盘；`Close()` 触发最终 flush。
  state 文件字段向后兼容（`views`/`hidden` 用 `omitempty`，旧文件可读）。
  未启用持久化时退化为纯内存，重启即失。

### 4.2 Provider（单文件获取）
- 职责：把 `(git_url, ref, filepath)` 解析成一次 HTTP 取文件请求，返回文件字节 + 元信息。
- **host 识别**（`IdentifyProvider`）：
  - 快速路径：URL 含 `github.com` → github；含 `gitlab.com`/`gitlab` → gitlab；
    含 `gitea` → gitea。零网络开销。
  - 回退路径：自托管实例（域名不含关键字）发 API 探测——
    `GET <scheme>://<host>/api/v1/version`（200 即 Gitea）、
    `GET <scheme>://<host>/api/v4/version`（200 即 GitLab）。
    探测正例按 host 缓存（`sync.Map`），同一 host 不重复探测；
    探测走 `m.client`（代理/超时生效），探测 URL 先过 SSRF 校验。
  - **不识别 host 直接报错**：未识别 host 不再尝试"通用 raw 模板回退"
    （原 `GenericProvider` 已删除），避免盲目请求任意 host 带来的 SSRF 与滥用风险。
- 按 host 选择具体 Provider：
  - **GitHub**：`https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{filepath}`
    （始终 https，与仓库 host scheme 无关），`Authorization: token` 头。
  - **GitLab**：API `GET /projects/{id}/repository/files/{path}/raw?ref=`，
    `PRIVATE-TOKEN` 头。
  - **Gitea**：`/{owner}/{repo}/raw/branch/{ref}/{filepath}`，
    `Authorization: token` 或 HTTP Basic。
- **GitHub URL 标准化**（`NormalizeGitHubURL`）：全项目唯一实现，registry/server 复用。
  从浏览器 URL 提取标准 git_url + ref，支持 `/tree/{ref}/path` 与 `/blob/{ref}/path`，
  缺省 ref 为 `main`，scheme 缺省 https。
- 鉴权：
  - 公开仓库：无凭据。
  - Token：放对应平台的请求头（`Authorization: token` / `PRIVATE-TOKEN`）。
  - 账号密码：HTTP Basic（用于支持 basic 的 raw 端点，如 Gitea）。
  - **404 语义区分**：GitHub/GitLab/Gitea 对私有仓库在无凭据时统一返回 404
    （不泄露仓库存在性）。`hasAuth` 区分：无凭据的 404 → `ErrAuthRequired`（触发
    前端凭据输入）；携带凭据仍 404 → `ErrNotFound`（真实不存在或凭据无权限）。
- **限流**（`getLimiter`）：按远端 host 分桶的令牌桶，默认 100 req/min/host
  （约 6000 req/hour），可配。避免对单一平台过度请求被对方封禁，也避免某平台
  限流影响其他平台。超限返回 `ErrRateLimited`，server 层映射为 HTTP 429。
- **代理**：`http.Transport.Proxy` 支持 HTTP/HTTPS 代理出网，便于内网部署访问
  公网 Git 平台。代理可来自配置或环境变量（`HTTP_PROXY`/`HTTPS_PROXY`）。
- **SSRF 可操作提示**（`SsrfHint`）：当 host 因解析到私网 IP 被 SSRF 拦截时，
  返回可操作的提示文案（"start server with --allow-host <host>"），
  把"unsupported git provider"这种含糊错误具体化。
- 约束：拉取超时、单文件大小上限（`io.LimitReader` 读时限制，非读后检查）。
  （原"限制重定向""仅允许 https"尚未实现，见 §8 与未实现清单。）
- 去重：对相同 `(providerType, gitURL, ref, filepath)` 的并发拉取用 singleflight 合并。

### 4.3 Cache（缓存）
- 进程内内存缓存：`hashicorp/golang-lru/v2` 真 LRU + TTL。
  键 `(pathid, filepath)`，值为渲染结果 + 拉取时间。
- 失效：TTL（可配，默认 60s）+ 容量上限（LRU 驱逐）；命中刷新，过期惰性删除。
- 手动失效接口配合 refresh API（按 pathid 前缀失效）。
- stale-while-revalidate 见 §13（未来扩展，未实现）。

### 4.4 Renderer（渲染）
- 按扩展名分派（`Kind` 是全项目唯一判定来源，provider 复用）：
  - `.html` / `.htm` / `.xhtml`：原样输出（仅注册仓库内容；安全见第 8 节）。
  - `.md` / `.markdown`：Markdown → HTML（`goldmark`，启用 GFM/表格/删除线/自动标题 ID）。
  - 纯文本/代码/配置（`.txt`/`.go`/`.py`/`.json`/... 一大批扩展名）：转义后放 `<pre><code>`。
  - 图片（`.png`/`.jpg`/...）：透传原始字节，前端 `<img>`。
  - 其它二进制：透传，前端提示下载。
  - 无扩展名的已知文本文件名（`README`/`Makefile`/`Dockerfile`/...）：当文本。
  - 文件不存在：404。
- **分页前端化**：服务端不做分页，长内容由 viewer 前端按行/高度切分
  （覆盖原"服务端分页"设计）。
- 统一外层模板：响应式布局、暗黑模式切换、语言切换（由 viewer.html 承载）。
- 注意：不重写文件内链接，相对链接可能失效——这是显式非目标。

### 4.5 前端（首页 + 站点外壳）
- **极简首页**（访问根 `/`）：
  - 输入框：粘贴 git 地址 → 可选填 pathid / ref → 提交注册，返回访问链接。
  - 已注册站点列表（可选，不含凭据）。
  - 极简但不简陋：留白、克制配色、清晰排版。
- **响应式**：移动端单列、桌面端居中限宽，纯 CSS。
- **暗黑模式**：默认跟随 `prefers-color-scheme`；手动切换按钮，选择存 `localStorage`，
  用 CSS 变量切换主题。
- **中英文**：按 `Accept-Language` 选默认 + 手动切换，文案存前端字典，选择存 `localStorage`。

## 5. HTTP 接口

| 方法   | 路径                       | 说明 |
|--------|----------------------------|------|
| GET    | `/`                        | 极简首页 |
| GET    | `/login`                   | 登录页（仅 `password` 配置非空时生效） |
| POST   | `/login`                   | 提交密码登录，成功下发会话 cookie |
| POST   | `/logout`                  | 注销会话（POST，非 GET，防 CSRF） |
| POST   | `/api/sites`               | 注册站点 `{git_url, pathid?, ref?, hidden?, auth?}`，返回 pathid 与访问 URL |
| GET    | `/api/sites`               | 列出公开站点（`Hidden=false`，不返回凭据明文，含 `views`） |
| DELETE | `/api/sites/:pathid`       | 注销站点 |
| POST   | `/api/sites/:pathid/refresh` | 失效该站点缓存 |
| PATCH  | `/api/sites/:pathid/hidden` | 切换站点可见性 `{hidden: bool}` |
| GET    | `/api/sites/:pathid/tree`  | 获取仓库可渲染文件树（可带 `Authorization` 访问私有仓库） |
| GET    | `/api/sites/:pathid/branches` | 获取仓库分支列表（用于 viewer 切换分支） |
| GET    | `/healthz`                 | 健康检查 |
| GET    | `/:pathid/*filepath`       | 拉取并返回站点内文件：md/txt/代码返回渲染 HTML 片段，html/图片/二进制透传原始字节；空路径返回 viewer 页面（Views+1） |

**注册鉴权**：服务作为完全公开服务，**不设注册鉴权**（无 `admin_token`）。
任何人都能注册与访问站点。

**全局访问密码（可选）**：若配置 `password` 非空，则所有页面（除 `/login`、
`/logout`、`/healthz`、`/static/*`）均需登录会话才可访问。会话 token 为 32 字节
随机值（`crypto/rand`），存 `sync.Map` + 过期时间（默认 30 天），cookie 带
`Secure` 标志（按请求 scheme 判断）、`HttpOnly`、`SameSite=Lax`。登录失败按 IP
限流（10 次失败锁 5 分钟）。未配置 `password` 时此层完全跳过，退化为公开服务。

**私有仓库凭据**：在**访问时运行时输入**：viewer 页面探测到远端 401/403（或无凭据的
404）后提示输入凭据，凭据只存浏览器 `sessionStorage`、随请求 `Authorization` 头带到
后端、用完即弃。同时同步写一份 base64 cookie 给后端兜底（浏览器导航、`<a>`、
`<img>`、`iframe src` 不带 `Authorization` 头但带同源 cookie）。详见第 8 节与第 12 节。

## 6. 配置

支持配置文件（YAML）与命令行/环境变量，命令行覆盖文件。配置文件支持环境变量插值
（`${VAR}`，避免明文写凭据）。

```yaml
base_url: https://gitweb.example.com   # 用于生成可访问链接，反代场景见 §8
listen: ":8080"
# password: "your-secret"              # 可选全局访问密码；留空 = 公开访问（默认）
cache:
  ttl: 60s
  max_entries: 2048
  max_file_size: 5242880               # 5MB，单文件大小上限
fetch:
  timeout: 10s
  http_proxy: ""                       # 出网代理；留空时回退 HTTP_PROXY 环境变量
  https_proxy: ""                      # 留空时回退 HTTPS_PROXY 环境变量
  # SSRF 白/黑名单（支持通配 *.gitea.*）；私网/环回/链路本地默认拒绝
  allow_hosts: []                      # 放行可信自托管内网实例（解析到私网 IP 时必须显式 allow）
  deny_hosts: []
  rate_limit: 100                      # 每分钟每个 host 允许的请求数，默认 100
sites:                                  # 启动时预置站点
  - git_url: https://github.com/username/helloworld
    pathid: mytest
    ref: main
    # hidden: true                     # 不进公开 /api/sites 列表；直链仍可访问
  - git_url: https://gitea.local/me/private-notes
    pathid: notes
    ref: main
    auth:
      type: token                      # token | basic
      token: ${NOTES_TOKEN}            # 环境变量插值，避免明文
```

命令行参数（覆盖配置文件）：

```
-config         配置文件路径
-listen         监听地址
-base-url       base URL
-password       全局访问密码
-http-proxy     HTTP 代理
-https-proxy    HTTPS 代理
-allow-host     追加 SSRF allow_hosts（逗号分隔，用于可信自托管内网实例）
-state          state 文件路径（默认 ./gitweb.state.json；空 = 纯内存）
-v              打印版本退出
```

state 文件：站点注册表 + 浏览数的持久化载体，启动加载、退出 flush、运行期每 5 秒
增量落盘。预置站点与 state 合并时，pathid 已存在则跳过（state 为运行时唯一真相源）。

## 7. 错误处理

- 远端取文件失败 / 鉴权失败：友好错误页，区分 404（文件不存在）与 502（远端异常），中英文。
- `pathid` 未注册：404 站点不存在页，首页引导注册。
- 文件超大：`io.LimitReader` 读时检测，超出返回 413；类型不支持预览：提示可下载。
- 远端超时：拉取超时返回 502。
- 渲染异常（md 解析失败）：返回 500 错误页（goldmark 解析失败极少见，降级纯文本非必要）。
- 平台限流（远端 429 或本地 host 限流命中）：本地限流返回 429 + 提示，缓存可缓解。
- 开放重定向：登录跳转等 `redirect` 参数校验，禁止以 `//` 或 `/\` 开头（防 open redirect）。

## 8. 安全考量

- **HTML 原样输出的 XSS**：渲染的是用户注册仓库的任意 HTML。由于全部站点同源
  （同一 `BASE_URL` 下的不同 path），站点间**不再有子域名隔离**。缓解：
  - viewer 页面响应设置严格 `Content-Security-Policy`，限制脚本来源。
  - `.html` 一律经 `<iframe sandbox="allow-same-origin">` 承载，公开部署默认
    **不加 `allow-scripts`**，防不可信仓库的恶意脚本执行。
  - 文档明确：开放给不可信仓库时，path 同源模型的隔离弱于子域名方案。
  - （注：若启用全局密码，登录会话 cookie 用 `HttpOnly`+`SameSite=Lax`+`Secure`，
    降低被同源脚本窃取风险；但不可信仓库 HTML 仍可发起同源 fetch——这是 path 同源
    模型的固有代价，敏感部署建议不启用密码层或仅在内网使用。）
- **注册 API 滥用**：服务作为完全公开服务不设注册鉴权；通过 SSRF 拦截、文件大小限制、
  host 限流、缓存容量与拉取超时控制资源滥用。
- **SSRF / 内网探测**：用户传入 git_url 可能指向内网。Provider 解析出的目标 host
  需经白名单/黑名单校验（`allow_hosts`/`deny_hosts`，支持通配），默认禁止解析到
  私网/环回/链路本地地址（除非显式 `allow_hosts` 放行，便于自建内网 git）。
  命中私网且未放行时，`SsrfHint` 返回可操作提示。
- **全局访问密码 / 会话**（可选，`password` 非空时启用）：
  - 会话 token 为 32 字节 `crypto/rand` 随机值，存 `sync.Map` + 过期时间（30 天）。
  - cookie：`HttpOnly`、`SameSite=Lax`、`Secure`（按请求 scheme，HTTPS 时为 true）。
  - 登录失败按 IP 限流：10 次失败锁该 IP 5 分钟。
  - logout 用 POST（非 GET，防 CSRF）。
- **凭据保护**（私有仓库运行时凭据）：
  - 主存浏览器 `sessionStorage`；持久化（勾选"记住"）时存 `localStorage` + 过期。
  - 每次请求经 `Authorization` 头带到后端、用完即弃；后端**不按 pathid 暂存、不落盘、
    不进日志、不进 List API**。
  - **兜底 cookie**：同步写一份 base64 cookie（`gitweb_repo_auth_<pathid>`），供浏览器
    导航（地址栏、`<a>`、`<img>`、`iframe src`）携带——这些请求不带 `Authorization`
    头但带同源 cookie，后端可从 cookie 恢复 auth。cookie 带 `SameSite=Lax`+`Secure`
    （按 `location.protocol`）。多用户访问同一 pathid 天然隔离（凭据只在各自浏览器）。
- **资源耗尽**：限制单文件大小（`io.LimitReader` 读时限制）、缓存容量（LRU 驱逐）、
  拉取超时、host 限流（100 req/min/host）。
- **重定向**：登录跳转等用户可控 URL 须校验不以 `//`/`\` 开头。
  （注：Provider 拉取远端时的 `http.Transport.CheckRedirect` 限制尚未实现，见未实现清单。）
- **反向代理**：`base_url` 作为服务根地址，用于生成可访问链接。`requestBaseURL()`
  优先取 `X-Forwarded-Proto` + `X-Forwarded-Host`，缺失时回退 `base_url`。
  cookie 的 `Secure` 标志也按 `X-Forwarded-Proto` 判断，确保反代场景正确。

## 9. 测试策略

- **单元测试**（已实现）：
  - Registry：注册/冲突/随机 pathid/校验/保留前缀/删除/`ListPublic` 排除 hidden。
  - Provider：URL 解析、`NormalizeGitHubURL`、SSRF 拒绝（私网/环回）、allow/deny 通配、
    401/403→`ErrAuthRequired`、无凭据 404→`ErrAuthRequired`、有凭据 404→`ErrNotFound`、
    大文件→`ErrTooLarge`、Gitea token 鉴权头、tree、`FilterRenderableFiles`、
    `IdentifyProvider` 探测与缓存。
  - Renderer：`Kind` 分派、`IsRenderable`、md/html/纯文本转义。
  - Cache：TTL 过期、LRU 驱逐、`Invalidate` 前缀。
- **集成测试**（待实现）：用 `httptest` 起模拟 raw/API 端点，端到端验证
  拉取 + 渲染 + 缓存 + 401/404/大文件/限流场景；HTTP 层验证 path 路由、保留前缀、
  管理 API、密码登录会话、SSRF 拒绝。
- **手动验证清单**：响应式断点、暗黑模式切换与持久化、中英文切换、长文档翻页、
  文件树浮窗停靠/拖拽/吸附、凭据弹窗与持久化。

## 10. 目录结构（建议）

```
gitweb/
  cmd/gitweb/main.go          # 入口：解析配置、启动 Gin
  internal/
    config/                   # 配置加载（文件 + 命令行 + 环境变量）
    registry/                 # 站点注册表
    provider/                 # github/gitlab/gitea 单文件获取 + 鉴权 + host 识别
    cache/                    # 内存缓存（TTL + LRU）
    render/                   # md/txt/html 渲染 + 分页
    server/                   # Gin 路由、handler、admin 鉴权、SSRF 校验
  web/
    templates/                # 站点外壳模板、首页模板、错误页
    static/                   # CSS、主题切换/语言切换的少量 JS
  docs/plans/                 # 设计文档
```

## 11. 关键依赖

- `github.com/gin-gonic/gin` — HTTP 框架。
- `net/http` — 拉取远端单文件（标准库即可，无需 go-git）。
- `github.com/yuin/goldmark` — Markdown 渲染。
- `github.com/hashicorp/golang-lru/v2` — LRU 缓存（或自实现）。
- `golang.org/x/sync/singleflight` — 并发拉取去重。

## 12. 已确认的决策

- **目录浏览**：提供文件树 API + viewer 页面常驻文件列表（覆盖原「不做目录浏览」决策）。
- **服务无注册鉴权**：作为完全公开服务，服务本身不设 `admin_token`、不做注册鉴权。
  任何人都能注册与访问。（覆盖原「动态注册需 admin_token」决策。）
- **可选全局访问密码**：部署方可配置 `password` 锁站。这是对"完全公开"的**可选收紧**，
  而非注册鉴权——它不区分用户角色，只区分"已登录/未登录"。会话用 cookie
  （`HttpOnly`+`SameSite=Lax`+`Secure`），登录失败按 IP 限流。
- **站点注册表与浏览数持久化**：通过 state 文件落盘，重启恢复；文件缓存仍为内存级。
  （覆盖原"服务进程无状态"表述——文件缓存确实无状态，但注册表已持久化。）
- **私有仓库凭据运行时输入**：不在注册时固定凭据（预置私有仓库除外）。访问页面时
  若仓库无公开访问权限（远端返回 401/403 或无凭据的 404），viewer 页面提示用户输入
  账号密码 / token，凭据只存浏览器 `sessionStorage`（持久化时存 `localStorage`），
  每次请求放 `Authorization` 头带到后端；后端用完即弃，**不按 pathid 暂存、不落盘、
  不进日志、不进 List API**。同时写一份 base64 cookie 兜底导航请求。多用户访问同一
  pathid 天然隔离（A 的凭据只在 A 的浏览器里）。
- **HTML 用 iframe sandbox 承载**：`.html` 当真实网页显示，但放
  `<iframe sandbox="allow-same-origin">`，公开部署默认不加 `allow-scripts`，
  防不可信仓库的恶意脚本逃逸。
- **分页前端化**：服务端不做分页，长内容由前端按行/高度切分（覆盖原「服务端分页」设计）。
- **SSRF 内网拦截**：默认拒绝私网/环回/链路本地，可配 `allow_hosts` 放行自建内网 git。
  命中时 `SsrfHint` 返回可操作提示。
- **凭据安全**：管理 API 不使用 cookie；凭据不进 URL、不进日志、不在 List 返回明文。
- **按 host 限流**：令牌桶 100 req/min/host，避免对单一 Git 平台过度请求被对方封禁。
- **站点可见性**：`Hidden` 字段控制是否进公开 `/api/sites` 列表；隐藏站点直链
  `/{pathid}/` 仍可访问。仅创建时可设，运行时可通过 PATCH 切换。
- **不识别 host 直接报错**：未识别 host 不做"通用 raw 模板回退"，直接返回错误
  （原 `GenericProvider` 已删除），避免盲目请求任意 host。
- **404 语义区分**：无凭据的 404 → `ErrAuthRequired`（触发前端凭据输入）；
  携带凭据的 404 → `ErrNotFound`（真实不存在）。

## 13. 未来扩展（本期不做）

- 增量/主动刷新升级为 webhook 触发。
- 外部缓存（Redis）支持多实例水平扩展。
- stale-while-revalidate：远端失败时回退旧缓存（私有仓库场景需注意凭据隔离）。
- 配置自定义 raw 模板规则（不识别 host 的通用回退，需配合严格 SSRF 白名单）。

## 14. 待实现清单（设计已写、代码未做）

按价值/成本排序：

1. **`loginFailures` 过期清理**（§8）：`sync.Map` 只增不删，长期运行内存泄漏。
   需后台 goroutine 定期清理过期条目（lockoutUntil 已过且超过宽限期的 entry）。
   **必要，需实现。**
2. **Provider 重定向限制**（§8）：`http.Transport.CheckRedirect` 限制跳转次数与
   目标 host，避免被引导到非预期 host。安全项，**建议实现。**
3. **集成测试**（§9）：server 层无测试，端到端覆盖拉取+渲染+缓存+鉴权+限流。
   质量保障，**建议实现。**
