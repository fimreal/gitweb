package registry

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"log"
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
	PathID    string
	GitURL    string
	Ref       string
	Auth      *Auth
	Provider  string
	CreatedAt time.Time
	Views     int64 // 站点被打开（viewer 页面）的次数；内存累加，防抖落盘
	Hidden    bool // 仅创建时可设。true=不进公开 /api/sites 列表，直链 /{pathid}/ 仍可访问
}

type Registry struct {
	mu         sync.RWMutex
	sites      map[string]*Site
	store      Store // nil = 纯内存（New() 默认）；非 nil 时 Register/Remove 原子落盘
	viewsDirty bool  // 浏览数有未落盘的累加；由 flush goroutine 定时写盘
	stopCh     chan struct{}
}

func New() *Registry {
	return &Registry{
		sites: make(map[string]*Site),
	}
}

// EnablePersistence 加载 path 处的 state 文件到内存（保留各站点 CreatedAt），
// 并挂上 store，使后续 Register/Remove 原子落盘。文件不存在视为空状态（非错误）。
// 应在启动时、注册 config 预置站点之前调用一次。
func (r *Registry) EnablePersistence(path string) error {
	st := newFileStore(path)
	records, err := st.Load()
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, rec := range records {
		// state 是首次注册时已校验过的可信数据，直接灌入 map，
		// 绕过 Register 以免加载本身触发 save、且避免重置 CreatedAt。
		r.sites[rec.PathID] = &Site{
			PathID:    rec.PathID,
			GitURL:    rec.GitURL,
			Ref:       rec.Ref,
			Provider:  rec.Provider,
			CreatedAt: rec.CreatedAt,
			Views:     rec.Views,
			Hidden:    rec.Hidden,
		}
	}
	r.store = st
	r.stopCh = make(chan struct{})
	// 防抖落盘：浏览数累加只置 dirty，由该 goroutine 每 5 秒 flush 一次，
	// 避免每次打开 viewer 都写盘。Close/Flush 时再确保写一次。
	go r.viewsFlushLoop(5 * time.Second)
	r.mu.Unlock()
	return nil
}

// viewsFlushLoop 周期性把未落盘的浏览数写盘，直到 Close。
func (r *Registry) viewsFlushLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.Flush()
		case <-r.stopCh:
			return
		}
	}
}

// Flush 立即把 dirty 的浏览数落盘（若 store 存在）。幂等。
func (r *Registry) Flush() {
	r.mu.Lock()
	dirty := r.viewsDirty
	r.viewsDirty = false
	store := r.store
	var snap []stateRecord
	if dirty && store != nil {
		snap = r.snapshot()
	}
	r.mu.Unlock()
	if snap != nil {
		if err := store.Save(snap); err != nil {
			log.Printf("registry: failed to persist views: %v", err)
		}
	}
}

// Close 停止 flush goroutine 并最后落盘一次。应在进程退出前调用。
func (r *Registry) Close() {
	r.mu.Lock()
	stop := r.stopCh
	r.stopCh = nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	r.Flush()
}

// IncrementViews 把某站点的浏览数 +1（内存累加，置 dirty，不立即写盘）。
// 站点不存在时静默忽略。
func (r *Registry) IncrementViews(pathID string) {
	r.mu.Lock()
	if s, ok := r.sites[pathID]; ok {
		s.Views++
		r.viewsDirty = true
	}
	r.mu.Unlock()
}

// snapshot 返回当前所有站点的落盘记录。调用方须持有 r.mu。
func (r *Registry) snapshot() []stateRecord {
	records := make([]stateRecord, 0, len(r.sites))
	for _, s := range r.sites {
		records = append(records, stateRecord{
			PathID:    s.PathID,
			GitURL:    s.GitURL,
			Ref:       s.Ref,
			Provider:  s.Provider,
			CreatedAt: s.CreatedAt,
			Views:     s.Views,
			Hidden:    s.Hidden,
		})
	}
	return records
}

// HasGitURL 报告是否已有站点使用该 gitURL（用于空 pathid 预置站点的去重）。
func (r *Registry) HasGitURL(gitURL string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sites {
		if s.GitURL == gitURL {
			return true
		}
	}
	return false
}

// reservedPrefixes 是不可用作 pathid 的前缀，避免与路由保留路径冲突。
var reservedPrefixes = map[string]bool{
	"api": true, "static": true, "healthz": true,
}

func (r *Registry) Register(gitURL, pathID, ref string, providerType string, auth *Auth, hidden bool) (*Site, error) {
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
		Hidden:    hidden,
	}

	r.sites[pathID] = site

	if r.store != nil {
		if err := r.store.Save(r.snapshot()); err != nil {
			// map 已改：落盘失败只降级记录，不回滚也不返回错误——
			// 否则调用方（handleRegisterSite）会报 400 但站点其实已注册，状态不一致。
			// 下次成功 save 会追上。
			log.Printf("registry: failed to persist state: %v", err)
		}
	}

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

// ListPublic 返回未标记为隐藏的站点，用于公开的 /api/sites 列表。
// 隐藏站点仍可用 Get + 直链访问，只是不进公开列表。
func (r *Registry) ListPublic() []*Site {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sites := make([]*Site, 0, len(r.sites))
	for _, s := range r.sites {
		if !s.Hidden {
			sites = append(sites, s)
		}
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

	if r.store != nil {
		if err := r.store.Save(r.snapshot()); err != nil {
			log.Printf("registry: failed to persist state: %v", err)
		}
	}

	return nil
}

// SetHidden 切换站点的公开/隐藏状态。hidden=true 时站点不进入 /api/sites 列表，
// 但直链 /{pathid}/ 仍可访问。变更后立即落盘。
func (r *Registry) SetHidden(pathID string, hidden bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	site, ok := r.sites[pathID]
	if !ok {
		return ErrNotFound
	}
	site.Hidden = hidden

	if r.store != nil {
		if err := r.store.Save(r.snapshot()); err != nil {
			log.Printf("registry: failed to persist state: %v", err)
		}
	}

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
