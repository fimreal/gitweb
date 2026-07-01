package registry

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrDuplicatePathID = errors.New("pathid already exists")
	ErrNotFound        = errors.New("site not found")
)

type Auth struct {
	Type     string
	Token    string
	Username string
	Password string
}

type Site struct {
	PathID     string
	GitURL     string
	Ref        string
	Auth       *Auth
	Provider   string
	DefaultDoc string
	CreatedAt  time.Time
}

type Registry struct {
	mu    sync.RWMutex
	sites map[string]*Site
}

func New() *Registry {
	return &Registry{
		sites: make(map[string]*Site),
	}
}

// reservedPrefixes 是不可用作 pathid 的前缀，避免与路由保留路径冲突。
var reservedPrefixes = map[string]bool{
	"api": true, "static": true, "healthz": true,
}

func (r *Registry) Register(gitURL, pathID, ref string, providerType string, auth *Auth) (*Site, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ref == "" {
		ref = "main"
	}

	if pathID == "" {
		pathID = generatePathID()
	} else if !IsValidPathID(pathID) {
		return nil, errors.New("invalid pathid: must be 1-32 chars of [a-zA-Z0-9_-]")
	} else if reservedPrefixes[pathID] {
		return nil, errors.New("pathid is reserved")
	}

	if _, exists := r.sites[pathID]; exists {
		return nil, ErrDuplicatePathID
	}

	site := &Site{
		PathID:    pathID,
		GitURL:    gitURL,
		Ref:       ref,
		Provider:  providerType,
		Auth:      auth,
		CreatedAt: time.Now(),
	}

	r.sites[pathID] = site

	return site, nil
}

func (r *Registry) Get(pathID string) (*Site, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	site, ok := r.sites[pathID]
	if !ok {
		return nil, ErrNotFound
	}
	return site, nil
}

func (r *Registry) List() []*Site {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sites := make([]*Site, 0, len(r.sites))
	for _, s := range r.sites {
		sites = append(sites, s)
	}
	return sites
}

func (r *Registry) Remove(pathID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sites[pathID]; !ok {
		return ErrNotFound
	}
	delete(r.sites, pathID)
	return nil
}

func generatePathID() string {
	b := make([]byte, 5)
	rand.Read(b)
	encoded := base32.StdEncoding.EncodeToString(b)
	encoded = strings.ToLower(encoded)
	encoded = strings.ReplaceAll(encoded, "=", "")
	if len(encoded) > 8 {
		encoded = encoded[:8]
	}
	return encoded
}

// IsValidPathID 校验自定义 pathid：字符集 [a-zA-Z0-9_-]、长度 1~32。
// 保留路径前缀（/api、/static、/healthz 等）由调用方在注册前检查，避免与保留前缀冲突。
func IsValidPathID(pathID string) bool {
	if len(pathID) == 0 || len(pathID) > 32 {
		return false
	}
	for _, r := range pathID {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
