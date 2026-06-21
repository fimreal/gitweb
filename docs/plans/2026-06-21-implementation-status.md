# gitweb 实现状态文档

更新时间：2026-06-21

## 与设计文档的主要差异

### 1. 文件树功能（已实现，设计文档说"不做"）

**设计文档原文：**
> 不做目录浏览：仅按确切 `filepath` 取单文件，不列举仓库目录。

**实际实现：**
- 提供了 `/api/sites/:pathid/tree` API，返回仓库完整文件树
- 实现了浮动文件树 UI：
  - 右下角气泡按钮（📁 图标）
  - 点击弹出可拖动浮动窗口
  - 支持文件夹展开/收起
  - 支持隐藏 `.` 开头文件的开关
  - 文件树支持缩进显示层级
  - 可缩小为气泡，不遮挡页面内容

**技术实现：**
- 使用 GitHub/GitLab/Gitea API 的 tree 端点获取完整文件列表
- 前端构建树形结构并渲染
- 支持点击文件直接跳转查看

### 2. 页面渲染方式

**设计文档隐含：** 服务端渲染 HTML 返回

**实际实现：**
- HTML 文件：服务端获取后原样返回，浏览器直接渲染
- Markdown/TXT：服务端转换为 HTML，嵌入统一的 viewer 模板
- Viewer 页面：Go template 渲染骨架，JavaScript 动态加载文件树和内容
- 文件内容获取：前端通过 API 请求文件树，通过路由获取文件内容

### 3. 暗黑模式实现细节

**实际实现：**
- 自动检测系统暗黑模式偏好（`prefers-color-scheme`）
- 用户首次访问时自动应用系统设置
- 手动切换后保存到 localStorage
- 监听系统暗黑模式变化，仅在用户未手动设置时自动切换
- viewer 页面和首页都支持暗黑模式

### 4. 文件类型支持

**实际支持的渲染类型：**
```go
".html", ".htm",           // 原样输出
".md", ".markdown",        // Markdown → HTML
".txt",                    // 纯文本 → HTML（保留换行）
".sh", ".bash", ".py",     // 脚本文件 → HTML（作为文本）
".js", ".css", ".json",    // 代码文件 → HTML（作为文本）
".xml", ".yaml", ".yml",   // 配置文件 → HTML（作为文本）
```

**二进制文件处理：**
- 检测文件开头字节判断是否为文本
- 大文件或二进制文件提示是否强制打开（设计中，待完善）

### 5. 分页功能

**实际实现：**
- 长文件自动分页（默认每页 1000 行）
- 页面底部显示翻页控件
- 通过 URL 参数 `?page=N` 控制页码

## 已实现的核心功能

### 基础架构 ✅
- Gin HTTP 服务器
- 内存 Registry（pathid → Site 映射）
- Provider 抽象层（GitHub/GitLab/Gitea）
- 内存缓存（文件内容）
- Markdown/TXT 渲染器

### 路由与访问 ✅
- `/:pathid/*filepath` 路由
- pathid 自定义或随机 8 位
- 默认 index.html
- 文件路径保留（如 `docs/test.md`）

### Git 仓库支持 ✅
- 公开仓库
- 私有仓库（token / 用户名密码）
- GitHub URL 规范化（处理 `/tree/branch/path` 格式）
- HTTP 代理支持（`--http-proxy` 参数）
- 多平台支持（GitHub/GitLab/Gitea）

### 前端 UI ✅
- 极简首页（DeepSeek 风格参考）
- 响应式布局
- 中英文双语切换（localStorage 持久化）
- 暗黑模式自动检测 + 手动切换
- 浮动文件树窗口（可拖动、缩小为气泡）

### API 接口 ✅
```
POST   /api/sites                    创建站点
GET    /api/sites                    列出所有站点
DELETE /api/sites/:pathid            删除站点
POST   /api/sites/:pathid/refresh    刷新缓存
GET    /api/sites/:pathid/tree       获取文件树
GET    /:pathid/*filepath            访问文件
```

## 未实现的设计功能

### 安全特性 ❌
- [ ] admin_token API 认证
- [ ] SSRF 防护（内网地址黑名单）
- [ ] Content-Security-Policy 响应头
- [ ] HTML 沙箱模式（iframe sandbox）
- [ ] 文件大小限制
- [ ] 并发请求限制

### 配置管理 ❌
- [ ] YAML 配置文件加载
- [ ] 环境变量插值（`${VAR}`）
- [ ] 命令行参数预置站点

### 错误处理 ⚠️
- [ ] 中英文友好错误页面
- [ ] 区分 404/502 错误
- [ ] 超时回退旧缓存
- [ ] 渲染失败降级纯文本

### 缓存优化 ⚠️
- [x] 基础内存缓存
- [ ] TTL 过期机制
- [ ] LRU 驱逐策略
- [ ] singleflight 防并发拉取

## 当前已知问题

1. **浮动窗口点击**：需要验证右下角气泡按钮点击是否正常工作
2. **文件树完整性**：需要验证是否列出所有文本文件（包括无后缀文件）
3. **HTML 文件渲染**：部分 HTML 文件加载显示 "Loading..." 问题（待定位）

## 技术栈

```
后端：
- Go 1.21+
- github.com/gin-gonic/gin
- github.com/yuin/goldmark (Markdown)
- net/http (标准库，拉取远端文件)

前端：
- 原生 JavaScript（无框架）
- CSS 变量实现主题切换
- 响应式布局（媒体查询）
```

## 启动方式

```bash
# 基础启动
./gitweb

# 带代理
./gitweb --http-proxy http://192.168.10.1:54122

# 指定端口和 base URL
./gitweb --port 8080 --base-url http://localhost:8080
```

## 使用流程

1. 访问首页 `http://localhost:8080/`
2. 输入 Git 仓库 URL（如 `https://github.com/user/repo`）
3. （可选）在高级选项中自定义 Path ID、分支、认证信息
4. 点击"创建"生成站点
5. 访问 `http://localhost:8080/<pathid>/` 查看内容
6. 点击右下角 📁 气泡打开文件树浏览其他文件

## 后续建议

### 高优先级
1. 修复浮动窗口点击问题（如果存在）
2. 添加 admin_token 保护 API
3. 实现文件大小限制
4. 改进错误页面

### 中优先级
5. 配置文件支持
6. SSRF 防护
7. 完善缓存机制（TTL + LRU）

### 低优先级
8. CSP 安全头
9. HTML 沙箱模式
10. 日志增强
