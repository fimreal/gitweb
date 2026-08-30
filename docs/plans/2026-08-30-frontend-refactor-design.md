# gitweb 前端重构设计

日期：2026-08-30　状态：已批准，进行中
关联：`docs/plans/2026-06-15-gitweb-design.md`（总体设计）

## 1. 背景

现有前端观感陈旧且内部自相矛盾：同一套深浅色逻辑在三个文件里各写一遍、两套色阶并存、毛玻璃与黑色投影被当作分层手段。本文记录重构的约束、决策与分阶段计划。

## 2. 硬约束

- **零构建、单二进制**。不引入 `package.json`、打包器、CDN、前端框架；所有 css/js/模板继续走 `//go:embed`。
- 因此样式只能手写，验证只能靠 `go build` + 重启服务 + 浏览器实测。**改任何模板/CSS/JS 都必须重新编译并重启才能看到效果**。

## 3. 已定决策

| 决策 | 内容 | 出处 |
| --- | --- | --- |
| 审美方向 | 克制的工程工具风（Linear / Vercel / GitHub 那一类）：显式边框 + 中性色阶 + 单一强调色 + 紧凑字号，去掉毛玻璃、光晕斑与重阴影 | 用户选定 |
| 首页 hero | 保留结构，只修细节 | 用户选定 |
| 文件树 | 浮动与侧栏两种形态都支持，加显式开关 | 用户选定 |
| 交付方式 | 一次性做完（视觉 + 模板 + 脚本），不分两期 | 用户选定 |
| 基准字号 | 16 → **14** | 用户，2026-08-30 |
| 模板复用 | **只抽片段，不做整页 base 继承** | 见下 |

### 3.1 为什么不做整页继承

`main.go` 把 `templates/*.html` 一次性 `ParseFS` 进**同一个** `html/template` set。实测（临时 Go 程序，两页都 `{{define "content"}}`）：后解析的定义静默覆盖前者，渲染 A 页输出的却是 B 页正文，**不报错**。动态 `{{template .Name}}` 派发本身可用，卡点是共享 set 里的名字冲突。

要躲开就得给每页建独立 set，并替换掉 Gin 的 `SetHTMLTemplate`、自写 `HTMLRender`。为省约 15 行 head 样板去动渲染主干，不划算；而片段已能覆盖全部真实重复。

## 4. 现状体检（可复现）

| 项 | 实测值 |
| --- | --- |
| `viewer.html` 总行数 / 内联 JS | 2237 / 1991 |
| CSS 十六进制色号 | 86 处，散在三个文件 |
| `--glass-*` 引用 | 50 处 |
| `backdrop-filter` | 28 处 |
| 暗色双写 `:root:not([data-theme="light"])` | 18 处（暗色机制共三套并存） |
| `data-en=` / `data-zh=` 对 | 59 对，另有 `=== 'zh'` 三元 70 处 |
| 内联 `<svg>` | 45 个 |
| 死引用 | `--panel-bg` / `--hover-bg` 仅在 `viewer.css:6-7` 定义，`style.css:660/671/684/691` 却在用，而首页不加载 `viewer.css` |
| 各页 head 资源 | 见 §5 阶段一表格，四张简单页互不一致 |

## 5. 阶段划分

顺序即依赖顺序。除最后一步外全是等价重构：任何一步出问题都能干净回滚，不牵连新功能。

### 阶段一 · 设计变量单一真源 + 模板骨架

新建 `web/static/css/tokens.css`，`style.css` / `viewer.css` / `toast.css` **只消费不定义**。

- 中性色阶统一到 slate（首页 `--fg:#0f172a` 即 slate-900；viewer 的 `#1a1a1a` 属 neutral —— 保留首页观感，改 viewer）
- 补齐缺失 token，上表那 4 处死引用自动痊愈
- 圆角收敛为一组紧凑刻度
- UI 基准字号 14；`viewer.css` 约 30 处硬编码 `px` 字号（跨度 10.5–18px）必须一并收进比例刻度，否则 14 只对首页生效
- 字体栈合并为 1 sans + 1 mono，**显式补 `PingFang SC` / `Microsoft YaHei`**（现在两套都不含中文栈，中文实际一直在吃浏览器 fallback）
- 暗色改为**边框主导**：暗色下 `--shadow-*` 设极弱值，靠 `1px solid var(--border)` 分界
- 主按钮在暗色下是 `--accent → --accent-hover(#60a5fa)` 的渐变，白字压在渐变亮端只有 **2.54:1**（AA 要 4.5:1）；改为 `#2563eb` 配白字实测 **5.17:1**
- 删除 `--glass-*` 与全部 `backdrop-filter`
- 三套暗色机制收敛为一套：只留 `[data-theme="dark"]`，删掉 `@media (prefers-color-scheme: dark)` 的 token 块与 18 处双写

模板侧新建 `web/templates/partials/`：`head.html`（参数化 title + 资源清单）、`icons.html`（45 个内联 SVG → 一个 sprite）、`controls.html`（三份复制的主题/语言按钮）、`modal.html`。`main.go` 的解析 pattern 增加一个目录。

顺带修的 4 个既有缺陷：`index.html:5` / `login.html:5` / `error.html:5` 缺 `viewport-fit=cover`；`error.html` 加载 `app.js` 却不加载 `toast.js`；§4 的 `--panel-bg` 死引用；`embed.html:8-16` 自带一段内联 `<style>`，与三个 css 文件并成第 4 个样式真源。

**完成判据**：`tokens.css` 之外 CSS 文件十六进制色号数 = 0；页面观感不变（截图对比）。

### 阶段二 · 拆 viewer 内联脚本

CSP 为 `script-src 'self' 'nonce-…'`，`'self'` 本就放开，外链脚本无需动 CSP，只有主题 bootstrap 那几行保留 nonce。按现有功能边界把 1991 行切成约 9 个文件放入 `static/js/viewer/`；浮窗拖拽/缩放/吸附/停靠那个文件**逐字搬运、不改一行逻辑**（本计划唯一高风险区恰好可以纯搬运）；裸全局收进 `window.GV` 命名空间。

**完成判据**：手工清单跑完浮窗 12 项 + 内容 6 项 + 凭据 5 项，全绿。

### 阶段三 · 文案集中

59 对 `data-en/data-zh` + 70 处 JS 三元 → 一个 `i18n.js` 字典（约 130 key，按 `tree.*` / `settings.*` / `auth.*` / `content.*` / `sites.*` 分命名空间）+ `t(key)` + `data-i18n`。附带收益：漏翻译从「静默」变成「可见」（key 缺失回落到 key 名）。

### 阶段四 · 视觉重绘

- 首页：hero 内容与站点卡片统一到同一 `--container: 680px` 轴（现在一个居中一个满宽）；`min-height:100dvh` 改上对齐 + 固定 padding，消掉首屏死白；卡片阴影减一档、加边框。
- 文件树：行高 ~44px → 28px；激活行从通铺改为 inset 圆角块；文件图标按类型分 4 色。
- 设置菜单：10 项按语义分 4 组、组间用留白不用分隔线；隐藏文件 / 行号两个状态项**加右侧开关**（现在它们长得跟动作项一样，看不出来可切换）。

### 阶段五 · 文件树侧栏开关（唯一的新增行为）

「固定为侧栏」开关，浮动 ↔ 侧栏两态。放最后，与前面所有等价重构隔离。

## 6. 防护措施

前端零测试，且零构建仓库里引 npm 测试栈是自找麻烦，所以约束用 **Go 测试正则守住**：禁止 `tokens.css` 之外出现色号、禁止 `backdrop-filter`、禁止 `data-en=` 残留。

已有的 `internal/server/` 表驱动冒烟（`server_test.go` / `assets_test.go` / `csp_test.go`）覆盖：每个路由渲染哪个模板、带哪些资源、资源是否 200、viewer 的 CSP nonce。

## 7. 已完成

`main` 上五笔（2026-08-30，未 push）：

1. `fix(security)` — ref 与 git_url 白名单校验、反代头防伪、站点卡片统一转义，附路由级回归测试
2. `refactor(ui)` — 三份重复的主题逻辑收拢为 `theme.js`，三态（跟随系统 / 浅色 / 深色），消除首帧闪烁
3. `fix(viewer)` — 停靠侧栏随页头收起联动上移、停靠标题栏回到天然高度、站点列表按钮双语提示
4. `test(server)` — viewer CSP nonce 基线
5. `style` — 全量 `gofmt`，纯空白，`gofmt -l .` 现已为空

## 8. 不在本次范围

- 私有仓库内 markdown 相对链接跳转 401：凭据只在客户端，用户判定为预期行为。
- `X-Forwarded-Host` 信任链、`IdentifyProvider` 的 `strings.Contains` 匹配：需要部署侧决策，另议。
