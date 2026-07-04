package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateRecord 是 state 文件中单个站点的记录。
// 注意：不持久化 Auth——运行时注册的站点 auth 恒为 nil（凭据只在浏览器
// sessionStorage），config 预置站点的 auth 每次启动从 config.yaml 重新加载。
// 因此 state 文件不含任何凭据。
type stateRecord struct {
	PathID    string    `json:"pathid"`
	GitURL    string    `json:"git_url"`
	Ref       string    `json:"ref"`
	Provider  string    `json:"provider"`
	CreatedAt time.Time `json:"created_at"`
	Views     int64     `json:"views,omitempty"`  // 旧 state 文件无此字段时反序列化为 0，向后兼容
	Hidden    bool      `json:"hidden,omitempty"` // 旧 state 文件无此字段时反序列化为 false（公开），向后兼容
}

type stateFile struct {
	Version int           `json:"version"`
	Sites   []stateRecord `json:"sites"`
}

// Store 是 registry 的可选持久化后端。nil 表示纯内存（New() 的默认行为）。
type Store interface {
	Load() ([]stateRecord, error)
	Save(records []stateRecord) error
}

type fileStore struct {
	path string
}

func newFileStore(path string) *fileStore {
	return &fileStore{path: path}
}

// Load 读取 state 文件。文件不存在时返回 nil, nil（非错误）。
func (f *fileStore) Load() ([]stateRecord, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", f.path, err)
	}
	return sf.Sites, nil
}

// Save 原子写入：先写同目录临时文件、chmod 0600，再 rename 替换，
// 避免崩溃时留下截断的半截文件。
func (f *fileStore) Save(records []stateRecord) error {
	data, err := json.MarshalIndent(stateFile{Version: 1, Sites: records}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".gitweb.state.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后为 no-op
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, f.path)
}
