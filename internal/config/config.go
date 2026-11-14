package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	BaseURL  string `yaml:"base_url"`
	Listen   string `yaml:"listen"`
	Password string `yaml:"password"` // 可选：若非空则所有页面需登录后才能访问
	// StateFile 是站点注册表落盘路径，对应命令行 -state / 环境变量 GITWEB_STATE。
	// 空值（不写这一行、写 ""、或 ${VAR} 未赋值展开成空）一律视为「未配置」，继续向下继承。
	// config 层刻意不提供关闭持久化的能力：本文件整份会过 os.ExpandEnv，把空串解读成「关掉」
	// 就等于让一个未赋值的变量静默丢掉全部站点。要关只能用 -state "" 或 GITWEB_STATE=""。
	StateFile string      `yaml:"state_file"`
	Cache     CacheConfig `yaml:"cache"`
	Fetch     FetchConfig `yaml:"fetch"`
	Sites     []SiteSpec  `yaml:"sites"`
}

type CacheConfig struct {
	TTL         time.Duration `yaml:"ttl"`
	MaxEntries  int           `yaml:"max_entries"`
	MaxFileSize int64         `yaml:"max_file_size"`
}

type FetchConfig struct {
	Timeout    time.Duration `yaml:"timeout"`
	HTTPProxy  string        `yaml:"http_proxy"`
	HTTPSProxy string        `yaml:"https_proxy"`
	AllowHosts []string      `yaml:"allow_hosts"`
	DenyHosts  []string      `yaml:"deny_hosts"`
	RateLimit  int           `yaml:"rate_limit"` // 每分钟每个 host 允许的请求数，默认 100
	// GitHubBaseURL 非空时启用 GitHub 企业版/自建实例地址（如 https://ghe.example.com）：
	// 文件走 <base>/{owner}/{repo}/raw/{ref}/{path}，API 走 <base>/api/v3。
	// 空值 = 公网 github.com：raw 用 raw.githubusercontent.com，API 用 api.github.com。
	GitHubBaseURL string `yaml:"github_base_url"`
}

type SiteSpec struct {
	GitURL string    `yaml:"git_url"`
	PathID string    `yaml:"pathid"`
	Ref    string    `yaml:"ref"`
	Hidden bool      `yaml:"hidden"`
	Auth   *AuthSpec `yaml:"auth,omitempty"`
}

type AuthSpec struct {
	Type     string `yaml:"type"`
	Token    string `yaml:"token,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	data = []byte(os.ExpandEnv(string(data)))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	setDefaults(&cfg)
	if err := cfg.Fetch.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate 在配置边界拦截会在运行期拼进 URL 的非法值：
// 一个带路径/查询串的 github_base_url 会让 raw/API 地址静默指向错误位置。
func (f FetchConfig) validate() error {
	if f.GitHubBaseURL == "" {
		return nil
	}
	u, err := url.Parse(f.GitHubBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("fetch.github_base_url must be an http(s) URL with a host: %q", f.GitHubBaseURL)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("fetch.github_base_url must not contain a path, query or fragment: %q", f.GitHubBaseURL)
	}
	return nil
}

func setDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.Cache.TTL == 0 {
		cfg.Cache.TTL = 60 * time.Second
	}
	if cfg.Cache.MaxEntries == 0 {
		cfg.Cache.MaxEntries = 2048
	}
	if cfg.Cache.MaxFileSize == 0 {
		cfg.Cache.MaxFileSize = 5 * 1024 * 1024
	}
	if cfg.Fetch.Timeout == 0 {
		cfg.Fetch.Timeout = 30 * time.Second
	}
	if cfg.Fetch.RateLimit == 0 {
		cfg.Fetch.RateLimit = 100 // 默认 100 req/min per host
	}

	// 从环境变量读取代理（如果配置文件未指定）
	if cfg.Fetch.HTTPProxy == "" {
		cfg.Fetch.HTTPProxy = os.Getenv("HTTP_PROXY")
		if cfg.Fetch.HTTPProxy == "" {
			cfg.Fetch.HTTPProxy = os.Getenv("http_proxy")
		}
	}
	if cfg.Fetch.HTTPSProxy == "" {
		cfg.Fetch.HTTPSProxy = os.Getenv("HTTPS_PROXY")
		if cfg.Fetch.HTTPSProxy == "" {
			cfg.Fetch.HTTPSProxy = os.Getenv("https_proxy")
		}
	}

	for i := range cfg.Sites {
		if cfg.Sites[i].Ref == "" {
			cfg.Sites[i].Ref = "main"
		}
	}
}
