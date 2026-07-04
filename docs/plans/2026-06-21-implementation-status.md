# gitweb 实现状态文档

更新时间：2026-07-03（浏览数统计 + 首页站点列表浮层）

## 本期重构要点

本次重构按 `docs/plans/2026-06-15-gitweb-design.md` 第 12 节「已确认的决策」收口，
核心变化：

1. **服务改为完全公开，无注册鉴权**：删除 `admin_token` / `authMiddleware` 整层。
   任何人都能注册与访问。滥用由 SSRF 拦截、文件大小限制、缓存容量与拉取超时兜底。
2. **私有仓库凭据改为运行时输入**：访问页面时若远端返回 401/403，viewer 弹窗提示
   输入 token / 账号密码。凭据只存浏览器 `sessionStorage`，每次请求经 `Authorization`
   头带到后端、用完即弃，**不落盘、不进日志、不进 List API**。多用户访问同一 pathid
   天然隔离（凭据只在各自浏览器）。
3. **渲染管线统一为前端 SPA**：viewer.html 是唯一入口；服务端 `wrapContent` 删除，
   `/:pathid/*filepath` 对 md/txt 返回渲染 HTML 片段，html/图片/二进制透传原始字节。
4. **HTML 用 iframe sandbox 承载**：`<iframe sandbox="allow-same-origin">`（不加
   `allow-scripts`），防不可信仓库的恶意脚本逃逸。
5. **分页前端化**：服务端不再分页（删掉假分页），长纯文本由前端按行切分。

## 已实现

### 基础架构
- Gin HTTP 服务器；`/:pathid/*filepath` 路由（空路径返回 viewer，有路径返回文件）
- 内存 Registry（pathid → Site），自定义 pathid 校验 + 保留前缀检查（api/static/healthz）
- Provider 抽象（GitHub/GitLab/Gitea），URL scheme 跟随输入（支持自建 http Gitea）
- 内存缓存：**hashicorp/golang-lru/v2 真 LRU + TTL**，命中刷新，过期惰性删除
- Markdown/HTML/text 渲染（goldmark），扩展名表全项目唯一（render.IsRenderable）

### 安全（公开部署硬需求）
- **SSRF**：`internal/provider/ssrf.go`，默认拒绝私网/环回/链路本地，复用 `allow_hosts`/`deny_hosts`（支持通配）
- **文件大小**：`io.LimitReader` 在读时限制（非读后检查），超出返回 `ErrTooLarge`
- **CSP**：viewer 页面与文件响应下发严格 `Content-Security-Policy`
- **HTML 沙箱**：`<iframe sandbox="allow-same-origin">`，无 `allow-scripts`
- **凭据安全**：仅 sessionStorage，不落盘/不进日志/List

### 路由与访问
- `/:pathid/*filepath`：空路径 → viewer 骨架（注入 RepoName/PathID/GitURL/Ref）；有路径 → 渲染片段或原始字节
- pathid 自定义（校验 `[a-zA-Z0-9_-]`、1~32 字符、非保留前缀）或随机 8 位
- 首屏自动加载 README.md（回退 index.html / README.txt / README）
- GitHub 浏览器 URL 标准化（支持 `/tree/{ref}/path`），`provider.NormalizeGitHubURL` 全项目唯一

### 前端 UI
- viewer：浮动可拖拽文件树窗口 + 气泡按钮；暗黑模式（CSS 变量）；凭据输入弹窗
- 首页：注册表单（无凭据字段，凭据运行时输入）；中英双语；暗黑模式
- 长纯文本前端分页
- **首页右下角站点列表入口按钮**：点击弹出玻璃浮层，内含搜索框（pathid/url/ref/provider 实时过滤）+ 翻页（每页 5 条）+ 站点卡片列表。浮层数据打开时按需 `fetch /api/sites` 加载。
- **viewer 标题栏浏览数徽章**：repo-ref 旁显示「👁 N」，N 为该站点累计被打开次数。

### 站点浏览数统计
- `Site` 结构体新增 `Views int64`；`stateRecord` 新增 `views` 字段（`omitempty`，向后兼容旧 state 文件）。
- 每次 `handleSiteFile` 空 filepath 分支（即打开 viewer 页面）调用 `registry.IncrementViews(pathid)`，内存累加 + 置 dirty，**不立即写盘**。
- `Registry` 启用持久化时开一个 flush goroutine，每 5 秒把 dirty 浏览数落盘一次，避免每次访问写盘。
- `main.go` 改为 `http.Server` + `signal.Notify(SIGINT/SIGTERM)` 优雅退出：收到信号后 `Shutdown(ctx)` 再 `reg.Close()`（最后 flush 一次），重启不丢浏览数。
- `GET /api/sites` 响应增加 `"views"` 字段。

### API
```
POST   /api/sites                    创建站点（无鉴权）
GET    /api/sites                    列出所有站点（含 views 浏览数）
DELETE /api/sites/:pathid            删除站点
POST   /api/sites/:pathid/refresh    刷新缓存
GET    /api/sites/:pathid/tree       获取文件树（可带 Authorization 访问私有仓库）
GET    /:pathid/*filepath            viewer 页面（Views+1）/ 文件内容
```

### 测试
- `internal/registry`：注册/冲突/随机 pathid/校验/保留前缀/删除
- `internal/provider`：URL 解析、SSRF 拒绝（私网/环回）、allow/deny 通配、401/403→ErrAuthRequired、404→ErrNotFound、大文件→ErrTooLarge、Gitea token 鉴权头、tree、FilterRenderableFiles
- `internal/cache`：TTL 过期、LRU 驱逐、Invalidate 前缀
- `internal/render`：Kind 分派、IsRenderable、md/html/纯文本转义

## 已知限制 / 待改进

- **GenericProvider**：未识别 host 仍返回 "not yet implemented"（设计提到的通用 raw 模板回退未做）。
- **缓存与私有仓库**：缓存按 `pathid:filepath` 共享，未按凭据隔离（YAGNI；公开仓库内容一致，私有仓库内容也按 pathid 共享——若需严格隔离可后续把缓存限定为公开仓库）。
- **GitHub 默认分支**：ref 默认 `main`，若仓库默认分支非 main 且未指定 ref，会 404；前端提示在文件树上方指定分支（未实现分支输入 UI）。
- **gitweb.log / 旧二进制**：仓库根有历史 `gitweb` 二进制与日志文件，不影响运行。
- **网络环境**：`raw.githubusercontent.com` 在部分网络环境下不可达（非代码问题）；tree 接口走 `api.github.com` 通常可达。

## 技术栈

```
后端：Go 1.21+ / gin / goldmark / hashicorp/golang-lru/v2 / golang.org/x/sync/singleflight
前端：原生 JS（无框架）/ CSS 变量
```

## 启动

```bash
go build -o gitweb ./cmd/gitweb
./gitweb                      # 默认 :8080
./gitweb --config config.yaml
./gitweb --http-proxy http://10.0.0.1:7890
```
