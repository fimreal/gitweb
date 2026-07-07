package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	BaseURL  string      `yaml:"base_url"`
	Listen   string      `yaml:"listen"`
	Password string      `yaml:"password"` // 可选：若非空则所有页面需登录后才能访问
	Cache    CacheConfig `yaml:"cache"`
	Fetch    FetchConfig `yaml:"fetch"`
	Sites    []SiteSpec  `yaml:"sites"`
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
}

type SiteSpec struct {
	GitURL  string    `yaml:"git_url"`
	PathID  string    `yaml:"pathid"`
	Ref     string    `yaml:"ref"`
	Hidden  bool      `yaml:"hidden"`
	Auth    *AuthSpec `yaml:"auth,omitempty"`
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
	return &cfg, nil
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
		cfg.Fetch.Timeout = 10 * time.Second
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
