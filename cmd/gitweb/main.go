package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/fimreal/gitweb/internal/cache"
	"github.com/fimreal/gitweb/internal/config"
	"github.com/fimreal/gitweb/internal/provider"
	"github.com/fimreal/gitweb/internal/registry"
	"github.com/fimreal/gitweb/internal/render"
	"github.com/fimreal/gitweb/internal/server"
	"github.com/fimreal/gitweb/internal/upgrade"
	"github.com/fimreal/gitweb/web"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入，默认 dev。
var version = "dev"

func main() {
	// upgrade 是唯一的子命令，必须在 flag.Parse 之前分发。
	// 版本号只出现在确认提示与成功摘要里，错误信息仍不带路径/URL/底层错误。
	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		runUpgrade(os.Args[2:])
		return
	}

	configPath := flag.String("config", "", "path to config file")
	listen := flag.String("listen", "", "listen address (overrides config)")
	baseURL := flag.String("base-url", "", "base URL (overrides config)")
	password := flag.String("password", "", "access password/key (overrides config); if set, all pages require login")
	httpProxy := flag.String("http-proxy", "", "HTTP proxy (overrides config and env)")
	httpsProxy := flag.String("https-proxy", "", "HTTPS proxy (overrides config and env)")
	statePath := flag.String("state", "", "path to state file for persisted sites; empty = in-memory only (overrides config state_file; default: $GITWEB_STATE, else ./gitweb.state.json)")
	allowHosts := flag.String("allow-host", "", "comma-separated extra hosts to allow through SSRF (e.g. git.example.com); for trusted self-hosted instances that resolve to private IPs")
	showVersion := flag.Bool("v", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: gitweb [flags]")
		fmt.Fprintln(flag.CommandLine.Output(), "       gitweb upgrade [--yes] [--version <tag>]   (replace this binary with a release)")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		log.Printf("gitweb %s", version)
		return
	}

	var cfg *config.Config
	var err error

	if *configPath != "" {
		cfg, err = config.Load(*configPath)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		cfg = &config.Config{
			Listen:  ":8080",
			BaseURL: "http://localhost:8080",
			Cache: config.CacheConfig{
				TTL:         60 * time.Second,
				MaxEntries:  2048,
				MaxFileSize: 5 * 1024 * 1024,
			},
			Fetch: config.FetchConfig{
				Timeout: 30 * time.Second,
			},
		}
	}

	if *listen != "" {
		cfg.Listen = *listen
	}
	if *baseURL != "" {
		cfg.BaseURL = *baseURL
	}
	if *password != "" {
		cfg.Password = *password
	}
	if *httpProxy != "" {
		cfg.Fetch.HTTPProxy = *httpProxy
	}
	if *httpsProxy != "" {
		cfg.Fetch.HTTPSProxy = *httpsProxy
	}
	// CLI 追加的 allow-host（逗号分隔），与 config 的 allow_hosts 合并、去重。
	// 用于可信自托管实例（域名解析到私网 IP 时必须显式放行，否则 SSRF 拦截 → unsupported git provider）。
	if *allowHosts != "" {
		extra := strings.Split(*allowHosts, ",")
		for i := range extra {
			extra[i] = strings.TrimSpace(extra[i])
		}
		cfg.Fetch.AllowHosts = append(cfg.Fetch.AllowHosts, extra...)
	}

	// state 路径：显式 -state > config 的 state_file > GITWEB_STATE > 内置默认。
	envValue, envSet := os.LookupEnv("GITWEB_STATE")
	stateFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "state" {
			stateFlagSet = true
		}
	})
	stateFile := resolveStatePath(statePathSources{
		flagValue: *statePath,
		flagSet:   stateFlagSet,
		cfgValue:  cfg.StateFile,
		envValue:  envValue,
		envSet:    envSet,
	})

	reg := registry.New()
	if stateFile != "" {
		if err := reg.EnablePersistence(stateFile); err != nil {
			log.Fatalf("Failed to enable site persistence at %s: %v", stateFile, err)
		}
		// 「写到哪个绝对路径」与「以哪个 uid 写」是排查落盘失败时唯一缺的两项信息，
		// 而 state 允许是相对路径 —— 不打印就等于让运维靠猜。
		abs, err := filepath.Abs(stateFile)
		if err != nil {
			abs = stateFile
		}
		log.Printf("State file: %s (uid=%d)", abs, os.Getuid())
	} else {
		log.Printf("State persistence disabled - registered sites are lost on restart")
	}
	providerMgr := provider.NewManager(cfg.Fetch.Timeout, cfg.Fetch.HTTPProxy, cfg.Fetch.HTTPSProxy,
		cfg.Fetch.AllowHosts, cfg.Fetch.DenyHosts, cfg.Cache.MaxFileSize, cfg.Fetch.RateLimit,
		cfg.Fetch.GitHubBaseURL)

	// 凭据 overlay：state 从不保存凭据，config 的 sites[].auth 在每次请求时按 pathid 叠加回来。
	// 这是「state 已存在 → 预置站点被跳过」不再等于「私有仓库重启后 401」的关键：
	// 首次播种把凭据放进内存站点，重启后全靠这张表。
	reg.SetAuthOverlay(buildAuthOverlay(cfg.Sites))

	for _, siteSpec := range cfg.Sites {
		// 与已加载的 state 合并：预置站点若已存在则跳过，既不重复也不触发写入。
		// state 是运行时唯一真相源，config 只补种缺失的预置。
		if siteSpec.PathID != "" {
			if _, err := reg.Get(siteSpec.PathID); err == nil {
				log.Printf("Site %s already registered (from state), skipping preset", siteSpec.PathID)
				continue
			}
		} else if reg.HasGitURL(siteSpec.GitURL) {
			// 空 pathid 预置：避免每次重启都种一个随机 id 的重复站点。
			log.Printf("Site for %s already registered, skipping preset", siteSpec.GitURL)
			continue
		}

		auth := toRegistryAuth(siteSpec.Auth)

		providerType := providerMgr.IdentifyProvider(siteSpec.GitURL)

		site, err := reg.Register(siteSpec.GitURL, siteSpec.PathID, siteSpec.Ref, providerType, auth, siteSpec.Hidden)
		if err != nil {
			log.Printf("Warning: failed to register site %s: %v", siteSpec.GitURL, err)
		} else {
			log.Printf("Registered site: %s -> %s", site.PathID, site.GitURL)
		}
	}

	// 播种之后再检查：此时 state 站点与本次注册的预置都已就位，能区分「overlay 命中」与「配了凭据却用不上」。
	for _, w := range missingAuthWarnings(reg, cfg.Sites) {
		log.Print(w)
	}

	cacheMgr := cache.New(cfg.Cache.TTL, cfg.Cache.MaxEntries)
	renderer := render.New(cfg.Render.ExtraTextExts...)

	srv := server.New(reg, providerMgr, cacheMgr, renderer, cfg.BaseURL, cfg.Password)

	r := srv.SetupRoutes()

	// 从嵌入的 web 目录加载模板与公共片段（不再依赖外部文件系统）。
	tpl, err := web.LoadTemplates()
	if err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}
	r.SetHTMLTemplate(tpl)

	// 静态资源也从嵌入 FS serve（去掉一层 static/ 前缀）。
	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		log.Fatalf("Failed to mount static FS: %v", err)
	}
	r.StaticFS("/static", http.FS(staticFS))

	log.Printf("Starting gitweb on %s", cfg.Listen)
	log.Printf("Base URL: %s", cfg.BaseURL)
	if cfg.Fetch.HTTPProxy != "" {
		log.Printf("HTTP Proxy: %s", cfg.Fetch.HTTPProxy)
	}
	if cfg.Fetch.HTTPSProxy != "" {
		log.Printf("HTTPS Proxy: %s", cfg.Fetch.HTTPSProxy)
	}

	// 优雅退出：收到 SIGINT/SIGTERM 后 Shutdown HTTP server，
	// 再 reg.Close() 把未落盘的浏览数 flush 到 state 文件，避免重启丢失。
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: r}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	reg.Close()
}

// upgradeCommand 把 gitweb upgrade 的一次执行抽成可注入依赖的结构：标准输入输出、
// 是否 TTY、当前版本与网络/安装参数都可替换，方便测试确认流程。
type upgradeCommand struct {
	opts    upgrade.Options
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	isTTY   bool
	version string // 当前二进制版本，例如 v0.6.2；无法确定时为 "dev"
}

// runUpgrade 执行自更新。默认源仓库公开，无需凭据；GITHUB_TOKEN 可选
// （提 API 额度，源仓库覆盖为私有时必填），源仓库（owner/name）可用
// GITWEB_UPGRADE_REPO 覆盖；凭据不做成 flag，避免 token 出现在 shell
// 历史与进程列表里。
func runUpgrade(args []string) {
	cmd := upgradeCommand{
		opts: upgrade.Options{
			Token: os.Getenv("GITHUB_TOKEN"),
			Repo:  os.Getenv("GITWEB_UPGRADE_REPO"),
		},
		stdin:   os.Stdin,
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		isTTY:   isTerminal(os.Stdin),
		version: currentVersion(),
	}
	os.Exit(cmd.run(args))
}

// run 执行一次升级，返回进程退出码。
func (c upgradeCommand) run(args []string) int {
	fs := flag.NewFlagSet("gitweb upgrade", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	var yes bool
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt (shorthand)")
	fs.StringVar(&c.opts.Tag, "version", "", "target release tag, e.g. v0.6.1; a lower tag is a downgrade")
	fs.Usage = func() {
		fmt.Fprintln(c.stderr, "Usage: gitweb upgrade [--yes] [--version <tag>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 2
	}

	// 非交互又没带 --yes：直接拒绝，连网络请求都不发 —— 不能替用户默认装。
	if !yes && !c.isTTY {
		fmt.Fprintln(c.stderr, "Refusing to upgrade without confirmation: standard input is not a terminal.")
		fmt.Fprintln(c.stderr, "Re-run with --yes to confirm non-interactively.")
		return 1
	}

	rel, err := upgrade.Resolve(context.Background(), c.opts)
	if err != nil {
		if errors.Is(err, upgrade.ErrReleaseNotFound) {
			fmt.Fprintf(c.stderr, "Upgrade failed: no release named %s on the update server.\n", strings.TrimSpace(c.opts.Tag))
			return 1
		}
		fmt.Fprintf(c.stderr, "Upgrade failed: %v\n", err)
		return 1
	}

	downgrade, same := classifyTarget(rel.Tag, c.version)
	// 版本降级只允许显式 --version 指定：latest 比当前旧说明源站没有更新的版本，
	// 不该悄悄把用户装回旧版。
	if downgrade && strings.TrimSpace(c.opts.Tag) == "" {
		fmt.Fprintf(c.stderr, "Upgrade failed: the latest release (%s) is older than the installed version (%s).\n", rel.Tag, c.version)
		fmt.Fprintln(c.stderr, "Pass --version <tag> to downgrade explicitly.")
		return 1
	}

	if !yes {
		if !c.confirm(rel.Tag, downgrade, same) {
			fmt.Fprintln(c.stdout, "Upgrade cancelled.")
			return 1
		}
	}

	verb := "Upgrading"
	if downgrade {
		verb = "Downgrading"
	}
	fmt.Fprintf(c.stdout, "%s to %s...\n", verb, rel.Tag)
	if err := upgrade.Install(context.Background(), c.opts, rel); err != nil {
		fmt.Fprintf(c.stderr, "Upgrade failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(c.stdout, "Done: %s -> %s. Restart gitweb to run the new binary.\n", c.version, rel.Tag)
	return 0
}

// confirm 打印「当前版本 → 目标版本」并读一行确认，默认选 No。
func (c upgradeCommand) confirm(target string, downgrade, same bool) bool {
	fmt.Fprintf(c.stdout, "Current version: %s\n", c.version)
	switch {
	case downgrade:
		fmt.Fprintf(c.stdout, "Target version:  %s (downgrade)\n", target)
		fmt.Fprintln(c.stdout, "This replaces the installed binary with an older release.")
	case same:
		fmt.Fprintf(c.stdout, "Target version:  %s (same version, reinstall)\n", target)
	default:
		fmt.Fprintf(c.stdout, "Target version:  %s\n", target)
	}
	verb := "upgrade"
	if downgrade {
		verb = "downgrade"
	}
	fmt.Fprintf(c.stdout, "Proceed with %s? [y/N]: ", verb)

	answer, err := bufio.NewReader(c.stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(answer) == "" {
		fmt.Fprintln(c.stdout)
		return false // EOF / 读错误：按默认 No 处理
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// classifyTarget 判断目标版本相对当前是升级还是降级。任一方版本未知时两者都为 false。
func classifyTarget(target, current string) (downgrade, same bool) {
	cmp, ok := upgrade.CompareVersions(target, current)
	if !ok {
		return false, false
	}
	return cmp < 0, cmp == 0
}

// currentVersion 取当前二进制版本：优先编译期注入的 main.version（release 构建用
// -ldflags -X main.version=<tag> 注入），未注入时退回 debug.ReadBuildInfo 的模块版本
// （go install 构建），都拿不到才显示 dev。
func currentVersion() string {
	if v := normalizeVersion(version); v != "" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := normalizeVersion(info.Main.Version); v != "" {
			return v
		}
	}
	return "dev"
}

// normalizeVersion 把 "0.6.2"/"v0.6.2" 统一成 "v0.6.2"；无法当作版本号的返回空串。
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "", "dev", "devel", "(devel)", "unknown":
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// defaultStatePath 是 -state、config 的 state_file 与 GITWEB_STATE 都未指定时的落盘位置。
// 它是相对路径，实际落在哪取决于进程的工作目录，因此启动日志会打印解析后的绝对路径。
const defaultStatePath = "./gitweb.state.json"

// statePathSources 是 state 路径的四个来源，优先级从高到低排列。
// 用结构体而非位置参数：四个来源里混着 bool 与不同空值语义，按顺序传极易读错。
type statePathSources struct {
	flagValue string // -state 的值
	flagSet   bool   // 是否显式传了 -state（由 flag.Visit 判定）
	cfgValue  string // config.yaml 的 state_file；空 = 未配置，继续向下继承
	envValue  string // GITWEB_STATE 的值
	envSet    bool   // GITWEB_STATE 是否存在
}

// resolveStatePath 按「显式 -state > config state_file > GITWEB_STATE > 默认值」解析 state 路径。
// 只有 flag 与 env 两层用「是否显式给出」判定，因为空串在那两层表示「纯内存、不落盘」，
// 按值非空判会让关掉持久化的意图被下一层覆盖。config 层相反：空值只是未配置——本文件整份会过
// os.ExpandEnv，把 state_file: ${VAR} 未赋值的空串当成「关掉」就等于把静默丢站点重新请回来。
func resolveStatePath(s statePathSources) string {
	switch {
	case s.flagSet:
		return s.flagValue
	case s.cfgValue != "":
		return s.cfgValue
	case s.envSet:
		return s.envValue
	default:
		return defaultStatePath
	}
}

// toRegistryAuth 把 config 的 auth 转成 registry 的凭据结构；nil 保持 nil。
func toRegistryAuth(spec *config.AuthSpec) *registry.Auth {
	if spec == nil {
		return nil
	}
	return &registry.Auth{
		Type:     spec.Type,
		Token:    spec.Token,
		Username: spec.Username,
		Password: spec.Password,
	}
}

// buildAuthOverlay 从 config 的 sites[] 构造请求期凭据覆盖表（pathid → auth）。
//
// 匹配键只取 pathid，不取 git_url：
//   - pathid 是 /{pathid}/ 路由与 state 条目本来的唯一主键，跨重启稳定；
//   - git_url 会撞车 —— 同一仓库挂多个 ref，或运行期又注册了同址站点时，按 URL 兜底
//     会让一个没配凭据的站点凭空继承别人条目的 token；
//   - 没写 pathid 的预置条目无法参与覆盖（播种时拿到随机 id），由 missingAuthWarnings 提示。
func buildAuthOverlay(specs []config.SiteSpec) map[string]*registry.Auth {
	overlay := make(map[string]*registry.Auth)
	for _, spec := range specs {
		if spec.PathID == "" || spec.Auth == nil {
			continue
		}
		overlay[spec.PathID] = toRegistryAuth(spec.Auth)
	}
	return overlay
}

// missingAuthWarnings 返回「config 里配了凭据、站点却拿不到」的预置条目的告警文案。
// 写了 pathid 的条目已由 overlay 覆盖；漏网的只有两种情况：预置没写 pathid（首次播种
// 拿到随机 id，重启后既无法回填也命中不了 overlay），或写的 pathid 与 state 里的条目对不上。
// 两者在运行时都表现为上游 401，提前一条 warn 比让人对着「私有仓库读不到」排查便宜得多。
func missingAuthWarnings(reg *registry.Registry, specs []config.SiteSpec) []string {
	var warnings []string
	for _, spec := range specs {
		if spec.Auth == nil {
			continue
		}
		if spec.PathID != "" {
			if site, err := reg.Get(spec.PathID); err == nil && site.Auth != nil {
				continue // overlay 命中
			}
		}
		if !reg.HasGitURL(spec.GitURL) {
			continue // 站点不存在：要么已注册失败，要么还没被播种，别拿它吓人
		}
		warnings = append(warnings, fmt.Sprintf(
			"Warning: %s has auth in config but no registered site matches it by pathid (empty pathid presets get a random id at first seeding); add \"pathid\" to that config entry so the credential overlay can apply",
			spec.GitURL))
	}
	return warnings
}
