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
- 服务进程**无状态**：站点映射与文件缓存都在进程内存中，重启即失。

站点来源两类：

- **启动时预置**：命令行参数或配置文件指定一批 `git_url → pathid`。
- **运行时动态注册**：通过 HTTP API 在运行期添加新站点。

## 2. 目标与非目标

### 目标
- 把任意 git 仓库（公开 / 私有）的**单个文本文件**实时转化为可浏览网页。
- 支持主流托管平台：GitHub / GitLab / Gitea（含自建实例）。
- 私有仓库支持 token 与账号密码鉴权（用于 raw / API 请求头）。
- path 前缀路由：每个仓库绑定一个指定或随机 8 位 `pathid`。
- 内存缓存单文件内容与渲染结果，按 TTL 失效。
- 渲染 `.html`（原样）、`.txt` / `.md`（转为 HTML），长内容自动翻页。
- 首页极简风、响应式、中英文双语、暗黑模式自动/手动切换。

### 非目标（YAGNI）
- 不 clone / pull 整个仓库，不维护本地工作区。
- 不重写文件内的相对链接、`<img>`/`<script>`/`<a>` 等资源路径（不解决跳转）。
- 不做用户账户体系 / 权限管理。
- 不做持久化数据库（Redis 等留作未来扩展）。
- 不做 git 写操作；只读。
- 不做 commit 历史 / diff / blame 浏览。

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
2. 路由 `/:pathid/*filepath` 解析出 `pathid = ab12cd34`、`filepath = docs/a.md`。
3. 用 `pathid` 查 `Registry`，拿到 `Site`（git_url、ref、鉴权）。查不到 → 404。
4. `filepath` 为空或 `/` → 回退默认入口 `index.html`。
5. 向 `Cache` 查 `(pathid, filepath)` 渲染结果：命中且未过期 → 直接返回。
6. 未命中 → `Provider` 把 `(git_url, ref, filepath)` 解析成具体的 raw URL 或平台
   API 请求，带上鉴权头，HTTP 拉取**单个文件**字节。
7. `Renderer` 按扩展名渲染：`.html` 原样、`.md`/`.txt` 转 HTML，超长分页。
8. 结果写入 `Cache`，返回浏览器。

### 3.3 path 路由方案

- 配置 `BASE_PATH`（一般为根 `/`）。路由形如 `/:pathid/*filepath`。
- 保留前缀避免冲突：`/api/...`、`/static/...`、`/healthz`、根 `/`（首页）
  优先匹配，不被当作 `pathid`。
- `pathid`：自定义（校验字符集 `[a-zA-Z0-9_-]`、长度上限、唯一）或随机 8 位
  base32 串（去除易混字符）。
- 相比子域名方案，无需泛域名解析与通配证书，单证书 / 单域即可，部署更简单。

## 4. 核心模块

### 4.1 Registry（站点注册表）
- 数据结构：`map[string]*Site`，键为 `pathid`，配 `sync.RWMutex`。
- `Site` 字段：`PathID`、`GitURL`、`Ref`（分支/标签/commit，默认仓库默认分支）、
  `Auth`（类型 + 凭据）、`Provider`（github/gitlab/gitea，按 host 推断或显式指定）、
  `DefaultDoc`（默认 `index.html`）、`CreatedAt`。
- 操作：`Register`（pathid 冲突检测；未指定则随机生成）、`Get`、`List`、`Remove`。

### 4.2 Provider（单文件获取）
- 职责：把 `(git_url, ref, filepath)` 解析成一次 HTTP 取文件请求，返回文件字节 + 元信息。
- 按 host 选择具体 Provider：
  - **GitHub**：`https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{filepath}`；
    私有仓库走 contents API（`/repos/{o}/{r}/contents/{path}?ref=`）+ `Authorization: token`。
  - **GitLab**：API `GET /projects/{id}/repository/files/{path}/raw?ref=`，
    `PRIVATE-TOKEN` 头；公开项目也可走 raw 路径。
  - **Gitea**：`/{owner}/{repo}/raw/branch/{ref}/{filepath}`，或 API `/repos/.../raw/...`，
    `Authorization: token`。
  - **通用回退**：若 host 不识别，尝试按常见 raw 规则拼接（可在配置里给模板）。
- 鉴权：
  - 公开仓库：无凭据。
  - Token：放对应平台的请求头（`Authorization: token/Bearer` 或 `PRIVATE-TOKEN`）。
  - 账号密码：HTTP Basic（用于支持 basic 的 raw 端点）。
- 约束：拉取超时、单文件大小上限、限制重定向、仅允许 https（可配）。
- 去重：对相同 `(pathid, filepath)` 的并发拉取用 singleflight 合并。

### 4.3 Cache（缓存）
- 进程内内存缓存：键 `(pathid, filepath)`，值为文件原始字节 + 渲染结果 + 拉取时间。
- 失效：TTL（可配，默认如 60s）+ 容量上限（LRU 驱逐）。
- 手动失效接口配合 refresh API。
- 可选增强：stale-while-revalidate（远端失败时回退旧缓存）。

### 4.4 Renderer（渲染）
- 按扩展名分派：
  - `.html` / `.htm`：原样输出（仅注册仓库内容；安全见第 8 节）。
  - `.md` / `.markdown`：Markdown → HTML（`goldmark`），套站点模板。
  - `.txt` 及其他纯文本：转义后置于 `<pre>`，套模板。
  - 未知二进制 / 不支持类型：提示不支持预览（可给原始下载链接）。
  - 文件不存在：404。
- **分页**：渲染后内容按字符/块阈值切分，URL 携带 `?page=N`，
  页脚提供上一页/下一页/页码；分页边界按块（段落/行）对齐避免截断标签。
- 统一外层模板：响应式布局、暗黑模式切换、语言切换。
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
| POST   | `/api/sites`               | 注册站点 `{git_url, pathid?, ref?, auth?}`，返回 pathid 与访问 URL |
| GET    | `/api/sites`               | 列出已注册站点（不返回凭据明文） |
| DELETE | `/api/sites/:pathid`       | 注销站点 |
| POST   | `/api/sites/:pathid/refresh` | 失效该站点缓存 |
| GET    | `/healthz`                 | 健康检查 |
| GET    | `/api/sites/:pathid/tree`  | 获取仓库可渲染文件树 |
| GET    | `/:pathid/*filepath`       | 拉取并返回站点内文件：md/txt/代码返回渲染 HTML 片段，html/图片/二进制透传原始字节；空路径返回 viewer 页面 |

**注册鉴权**：服务作为完全公开服务，**不设注册鉴权**（无 `admin_token`）。
私有仓库的鉴权在**访问时运行时输入**：viewer 页面探测到远端 401/403 后提示输入凭据，
凭据只存浏览器 `sessionStorage`、随请求 `Authorization` 头带到后端、用完即弃。
详见第 8 节与第 12 节。

## 6. 配置

支持配置文件（YAML）与命令行/环境变量，命令行覆盖文件。

```yaml
base_url: https://gitweb.example.com   # 用于生成可访问链接
listen: ":8080"
cache:
  ttl: 60s
  max_entries: 2048
  max_file_size: 5MB
fetch:
  timeout: 10s
  https_only: true
admin_token: ""                        # 注册/管理 API 鉴权
sites:                                  # 启动时预置站点
  - git_url: https://github.com/username/helloworld
    pathid: mytest
    ref: main
  - git_url: https://gitea.local/me/private-notes
    pathid: notes
    ref: main
    auth:
      type: token                      # token | basic
      token: ${NOTES_TOKEN}            # 环境变量插值，避免明文
```

命令行示例：

```
gitweb --base-url https://gitweb.example.com \
       --site "https://github.com/username/helloworld#main=>mytest"
```

## 7. 错误处理

- 远端取文件失败 / 鉴权失败：友好错误页，区分 404（文件不存在）与 502（远端异常），中英文。
- `pathid` 未注册：404 站点不存在页，首页引导注册。
- 文件超大 / 类型不支持：413 / 提示不支持预览。
- 远端超时：设拉取超时，超时返回 502，可回退旧缓存。
- 渲染异常（md 解析失败）：降级纯文本并记录日志。
- 平台限流（API rate limit）：透传状态并提示，缓存可缓解。

## 8. 安全考量

- **HTML 原样输出的 XSS**：渲染的是用户注册仓库的任意 HTML。由于全部站点同源
  （同一 `BASE_URL` 下的不同 path），站点间**不再有子域名隔离**。缓解：
  - 服务无注册鉴权、不用 cookie 会话，不存在可被同源脚本窃取的管理会话。
  - viewer 页面响应设置严格 `Content-Security-Policy`，限制脚本来源。
  - `.html` 一律经 `<iframe sandbox>` 承载，公开部署默认**不加 `allow-scripts`**，
    防不可信仓库的恶意脚本执行；需要交互式 Demo 时再单独放宽并配独立 origin。
  - 文档明确：开放给不可信仓库时，path 同源模型的隔离弱于子域名方案。
- **注册 API 滥用**：服务作为完全公开服务不设注册鉴权；通过 SSRF 拦截、文件大小限制、
  缓存容量与拉取超时控制资源滥用（见下）。
- **SSRF / 内网探测**：用户传入 git_url 可能指向内网。Provider 解析出的目标 host
  需经白名单/黑名单校验，默认禁止解析到私网/环回地址（除非显式允许，便于自建内网 git）。
- **凭据保护**：私有凭据**只存浏览器 `sessionStorage`**，不落盘、不上后端常驻、
  不在 `List`/日志明文返回；每次请求经 `Authorization` 头带到后端、用完即弃。
- **资源耗尽**：限制单文件大小、并发拉取数、缓存容量、拉取超时。
- **重定向**：限制 / 禁止跨站重定向，避免被引导到非预期 host。

## 9. 测试策略

- **单元测试**：
  - Registry：注册/冲突/随机 pathid/删除。
  - Provider：各平台 URL/请求构造正确性、host 识别、鉴权头注入（用 `httptest` 模拟远端）。
  - Renderer：md/txt/html 渲染、分页边界、转义。
  - Cache：TTL 过期、LRU 驱逐、并发安全、singleflight 去重。
- **集成测试**：
  - 用 `httptest` 起一个模拟 raw / API 端点（含 401 / 404 / 大文件 / 限流场景），
    端到端验证拉取 + 渲染 + 缓存。
  - HTTP 层用 `httptest` 验证 path 路由、保留前缀、管理 API、admin_token 校验。
  - SSRF 校验：构造内网 host 的 git_url 应被拒绝。
- **手动验证清单**：响应式断点、暗黑模式切换与持久化、中英文切换、长文档翻页。

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
- **私有仓库凭据运行时输入**：不在注册时固定凭据。访问页面时若仓库无公开访问权限
  （远端返回 401/403），viewer 页面提示用户输入账号密码 / token，凭据只存浏览器
  `sessionStorage`，每次请求放 `Authorization` 头带到后端；后端用完即弃，**不按 pathid
  暂存、不落盘、不进日志、不进 List API**。多用户访问同一 pathid 天然隔离
  （A 的凭据只在 A 的浏览器里）。
- **HTML 用 iframe sandbox 承载**：`.html` 当真实网页显示，但放 `<iframe sandbox>`，
  公开部署默认不加 `allow-scripts`，防不可信仓库的恶意脚本逃逸。
- **分页前端化**：服务端不做分页，长内容由前端按行/高度切分（覆盖原「服务端分页」设计）。
- **SSRF 内网拦截**：默认拒绝私网/环回/链路本地，可配 `allow_hosts` 放行自建内网 git。
- **凭据安全**：管理 API 不使用 cookie；凭据不进 URL、不进日志、不在 List 返回明文。

## 13. 未来扩展（本期不做）

- 增量/主动刷新升级为 webhook 触发。
- 外部缓存（Redis）支持多实例水平扩展。
- 不识别 host 的通用 raw 模板规则，可在配置中开放自定义。
