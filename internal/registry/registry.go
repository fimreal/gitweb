package registry

import (
	"fmt"
	"net/url"
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

func (r *Registry) Register(gitURL, pathID, ref string, providerType string, auth *Auth) (*Site, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ref == "" {
		ref = "main"
	}

	if pathID == "" {
		pathID = generatePathID()
	}

	if _, exists := r.sites[pathID]; exists {
		return nil, errors.New("pathid already exists")
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

func isValidPathID(pathID string) bool {
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

func detectProvider(gitURL string) string {
	lower := strings.ToLower(gitURL)
	if strings.Contains(lower, "github.com") {
		return "github"
	}
	if strings.Contains(lower, "gitlab.com") || strings.Contains(lower, "gitlab") {
		return "gitlab"
	}
	if strings.Contains(lower, "gitea") {
		return "gitea"
	}
	return "generic"
}

// normalizeGitHubURL 从浏览器 URL 中提取标准 Git URL 和 ref
func normalizeGitHubURL(inputURL string) (gitURL, ref string, err error) {
	u, err := url.Parse(inputURL)
	if err != nil {
		return "", "", err
	}
	
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", errors.New("invalid github url")
	}
	
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	gitURL = fmt.Sprintf("https://%s/%s/%s", u.Host, owner, repo)
	
	// 检查是否包含 /tree/{ref} 或 /blob/{ref}
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		ref = parts[3]
	} else {
		ref = "main"
	}
	
	return gitURL, ref, nil
}
