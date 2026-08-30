package main

import (
	"context"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fimreal/gitweb/internal/cache"
	"github.com/fimreal/gitweb/internal/config"
	"github.com/fimreal/gitweb/internal/provider"
	"github.com/fimreal/gitweb/internal/registry"
	"github.com/fimreal/gitweb/internal/render"
	"github.com/fimreal/gitweb/internal/server"
	"github.com/fimreal/gitweb/web"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入，默认 dev。
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config file")
	listen := flag.String("listen", "", "listen address (overrides config)")
	baseURL := flag.String("base-url", "", "base URL (overrides config)")
	password := flag.String("password", "", "access password/key (overrides config); if set, all pages require login")
	httpProxy := flag.String("http-proxy", "", "HTTP proxy (overrides config and env)")
	httpsProxy := flag.String("https-proxy", "", "HTTPS proxy (overrides config and env)")
	statePath := flag.String("state", "", "path to state file for persisted sites; empty = in-memory only (default: $GITWEB_STATE, else ./gitweb.state.json)")
	allowHosts := flag.String("allow-host", "", "comma-separated extra hosts to allow through SSRF (e.g. git.example.com); for trusted self-hosted instances that resolve to private IPs")
	showVersion := flag.Bool("v", false, "print version and exit")
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

	// state 路径：显式 -state > GITWEB_STATE > 内置默认。
	envValue, envSet := os.LookupEnv("GITWEB_STATE")
	stateFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "state" {
			stateFlagSet = true
		}
	})
	stateFile := resolveStatePath(*statePath, stateFlagSet, envValue, envSet)

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
		cfg.Fetch.AllowHosts, cfg.Fetch.DenyHosts, cfg.Cache.MaxFileSize, cfg.Fetch.RateLimit)

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

		var auth *registry.Auth
		if siteSpec.Auth != nil {
			auth = &registry.Auth{
				Type:     siteSpec.Auth.Type,
				Token:    siteSpec.Auth.Token,
				Username: siteSpec.Auth.Username,
				Password: siteSpec.Auth.Password,
			}
		}

		providerType := providerMgr.IdentifyProvider(siteSpec.GitURL)

		site, err := reg.Register(siteSpec.GitURL, siteSpec.PathID, siteSpec.Ref, providerType, auth, siteSpec.Hidden)
		if err != nil {
			log.Printf("Warning: failed to register site %s: %v", siteSpec.GitURL, err)
		} else {
			log.Printf("Registered site: %s -> %s", site.PathID, site.GitURL)
		}
	}

	cacheMgr := cache.New(cfg.Cache.TTL, cfg.Cache.MaxEntries)
	renderer := render.New()

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

// defaultStatePath 是 -state 与 GITWEB_STATE 都未指定时的落盘位置。它是相对路径，
// 实际落在哪取决于进程的工作目录，因此启动日志会打印解析后的绝对路径。
const defaultStatePath = "./gitweb.state.json"

// resolveStatePath 按「显式 -state > GITWEB_STATE > 默认值」解析 state 路径。
// 空串在任一层都表示「纯内存、不落盘」，所以调用方须用 flag.Visit 区分「没传 -state」
// 与「传了 -state ""」——否则关持久化的意图会被 env 或默认值覆盖掉。
func resolveStatePath(flagValue string, flagSet bool, envValue string, envSet bool) string {
	if flagSet {
		return flagValue
	}
	if envSet {
		return envValue
	}
	return defaultStatePath
}
