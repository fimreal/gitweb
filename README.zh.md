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

### 自更新

```bash
gitweb upgrade
```

从源仓库所在的 Gitea 实例下载最新 release 并替换当前二进制。更新链路仅 HTTPS：API 请求、asset 下载与每一跳重定向都必须严格同源（scheme、主机、端口全等），降级与跨源目标一律拒绝。

替换前先校验下载产物：归档完整性（gzip 尾部校验和必查）、可执行格式、架构与头部表边界；若 release 附带 `<asset>.sha256` 校验和 asset 则强制比对。校验的边界要说清楚：**没有校验和时，实际检查的只有 gzip CRC、部分可执行结构和 TLS 传输链路，不保证文件完整、可运行**；即使有校验和，它与二进制来自同一源站，防的是传输损坏与源站被篡改后的不一致，**不是独立的发布者签名**。下载或校验的任何一步失败都保持原文件不动。

替换方式因平台而异：

- **Linux/macOS**：rename 原子覆盖旧二进制，运行中的进程不受影响。
- **Windows**：运行中的可执行文件无法覆盖，先把旧文件 rename 为 `<binary>.old`，再让新文件就位。这*不是*原子操作：第二步失败会回滚旧文件；回滚也失败时保留 `.old` 备份。若升级恰好在两次 rename 之间被中断，原文件名会缺失，恢复方式二选一：
  1. 手动把同目录下的 `gitweb.exe.old` 重命名回 `gitweb.exe` —— 不依赖网络，随时可用；
  2. 运行备份：`gitweb.exe.old upgrade` —— 本次升级成功时会顺带把备份还原回原名。注意还原发生在下载与校验全部通过之后：升级失败（比如源站上没有可用 release）时不会做任何还原，此时请用方式 1。

  升级成功后 `.old` 备份保留到下次升级确认可执行文件存在为止。

为满足「任何输出不含版本号」的约定，升级失败的报错只有固定分类文案，不携带路径、URL 或底层系统错误 —— 安装目录和下载链接都可能含版本号。代价是报错里拿不到具体路径；备份位置按约定告知：永远是可执行文件同目录下的 `<可执行文件名>.old`。

替换完成后重启 gitweb 即运行新二进制。

发布仓库为私有，需通过环境变量提供 Gitea access token：

```bash
GITWEB_UPGRADE_TOKEN=<token> gitweb upgrade
```

该命令只做更新替换，不输出任何版本信息。

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
  # GitHub Enterprise / 自建 GitHub：填实例根地址，如 https://ghe.example.com。
  # git_url 指向该 host 的仓库会从 <base>/{owner}/{repo}/raw/{ref}/{path} 取文件，
  # tree/branches 走 <base>/api/v3；github.com 仓库仍走公网地址。留空 = 公网 github.com。
  github_base_url: ""
  # SSRF 防护：不匹配 allow/deny 规则的私网主机默认会被拦截。
  allow_hosts: []
  deny_hosts: []

# 在内置可渲染文本扩展名表（文档/代码/脚本，含 .ps1/.psm1/.psd1/.bat/.cmd/.reg/.vbs）
# 之外追加扩展名：命中的文件按纯文本/代码渲染，且 ?raw=1 以 text/plain 返回原始源码。
# 取值可带或不带前导点，大小写不敏感；留空（默认）不追加任何扩展名。
# 注意上面七个 Windows 脚本扩展名已内置并按文本渲染 —— 这是相对旧版本的有意变化，
# 不因本项留空而回退。
render:
  extra_text_exts: []  # 例如 [".tf", "hcl"]

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

### Token scope

gitweb 只读仓库、从不写入，因此只读 token 就够用，也应该优先使用：

| 平台 | 只读 scope | 写入 scope（仅在别处需要 push 时） |
| --- | --- | --- |
| GitHub | fine-grained：Contents = Read；classic：`repo`（私有）/ `public_repo` | Contents = Read and write；classic `repo` |
| GitLab | `read_repository` | `write_repository` |
| Gitea | `read:repository` | `write:repository` |

只读 token 取文件正常返回 200，但用同一个 token **写文件**会被拒绝为 **403** —— Gitea 的
原文是 `required=[write:repository], token scope=read:repository`。如果读得通、写却 403，
先查 token scope，这是平台在执行最小权限，不是服务端 bug。

Gitea 的 `ref` 支持分支、tag、commit SHA 三种。Gitea raw 端点区分三者
（`/raw/branch/`、`/raw/tag/`、`/raw/commit/`），且不会在 `/raw/branch/` 下解析 SHA，
所以 gitweb 按 branch → tag → commit 依次探测，并把解析结果按仓库/ref 缓存。
因此固定到 tag 或 SHA 的站点可以正常访问，代价只是首次多一次请求。

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
- **provider**：从 GitHub/GitLab/Gitea 获取单个文件，按 URL scheme 路由，singleflight 去重（按凭据指纹隔离）
- **cache**：内存 LRU + TTL 缓存（hashicorp/golang-lru/v2）
- **render**：Markdown/HTML/文本 渲染 —— 可渲染文件类型的唯一判定来源
- **server**：Gin HTTP 服务器，包含路径路由、SSRF 防护、大小限制、CSP
- **upgrade**：自更新 —— 从源仓库的 Gitea release 下载，仅 HTTPS 严格同源链路，先校验后替换，Windows 带备份与回滚

## 安全

- **无注册鉴权**：公共服务。通过 SSRF 防护、文件大小限制、缓存/超时上限来约束滥用。
- **站点密码**（可选）：设置 `password`（配置文件或 `-password` 参数）后，所有页面都需要 Cookie 登录。`/login`、`/logout`、`/healthz` 和 `/static/*` 保持公开，以便登录页面正常渲染。
- **SSRF 防护**：`allow_hosts`/`deny_hosts`（支持通配符）；默认拦截私网、回环和链路本地 IP。
- **文件大小**：通过 `io.LimitReader` 流式读取，超大文件在占满内存前就会被拒绝。
- **限流**：按 host 分桶的令牌桶限流器，限制对外部 Git API 的请求（默认每个 host 100 req/min，可通过 `fetch.rate_limit` 配置）。防止滥用，并避免触及上游平台 API 限制（如 GitHub 未认证请求的 60 req/hour）。
- **CSP + 沙箱**：浏览页面下发严格的 Content-Security-Policy；用户 `.html` 在 `<iframe sandbox="allow-same-origin">`（不含 `allow-scripts`）中渲染，阻止不可信脚本执行。
- **凭据**：仅保存在浏览器（sessionStorage）中，随请求通过 `Authorization` 发送；服务端凭据仅用于当前请求，不写入缓存或持久化存储，不入日志，列表 API 也不会返回。
- **缓存隔离**：服务端内容缓存键混入本次请求所用凭据的截断 SHA-256 指纹（匿名请求使用固定标记 `anon`），因此私有仓库被带凭据抓取后，在 TTL 内不会被匿名或不同凭据的访客命中缓存读到；provider 层 singleflight 的并发合并键同样混入该指纹，不同凭据的并发请求也不会共享同一次上游抓取结果。凭据原文既不进缓存键，也不进缓存 value。

## 许可证

见 LICENSE 文件。
