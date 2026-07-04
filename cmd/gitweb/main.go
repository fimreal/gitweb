package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/limingrui/gitweb/internal/cache"
	"github.com/limingrui/gitweb/internal/config"
	"github.com/limingrui/gitweb/internal/provider"
	"github.com/limingrui/gitweb/internal/registry"
	"github.com/limingrui/gitweb/internal/render"
	"github.com/limingrui/gitweb/internal/server"
)

// version 在构建时通过 -ldflags "-X main.version=..." 注入，默认 dev。
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config file")
	listen := flag.String("listen", "", "listen address (overrides config)")
	baseURL := flag.String("base-url", "", "base URL (overrides config)")
	httpProxy := flag.String("http-proxy", "", "HTTP proxy (overrides config and env)")
	httpsProxy := flag.String("https-proxy", "", "HTTPS proxy (overrides config and env)")
	statePath := flag.String("state", "./gitweb.state.json", "path to state file for persisted sites (empty = in-memory only)")
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
				Timeout:   10 * time.Second,
				HTTPSOnly: false,
			},
		}
	}

	if *listen != "" {
		cfg.Listen = *listen
	}
	if *baseURL != "" {
		cfg.BaseURL = *baseURL
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

	reg := registry.New()
	if *statePath != "" {
		if err := reg.EnablePersistence(*statePath); err != nil {
			log.Fatalf("Failed to load state %s: %v", *statePath, err)
		}
	}
	providerMgr := provider.NewManager(cfg.Fetch.Timeout, cfg.Fetch.HTTPProxy, cfg.Fetch.HTTPSProxy,
		cfg.Fetch.AllowHosts, cfg.Fetch.DenyHosts, cfg.Cache.MaxFileSize)

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

	srv := server.New(reg, providerMgr, cacheMgr, renderer, cfg.BaseURL, cfg.Cache.MaxFileSize)

	r := srv.SetupRoutes()

	r.LoadHTMLGlob("web/templates/*")
	r.Static("/static", "web/static")

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
