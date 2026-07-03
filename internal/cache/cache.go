package cache

import (
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2"
)

// entry 缓存项：数据 + 过期时间。
type entry struct {
	data      []byte
	expireAt  time.Time
}

// Cache 是进程内 LRU + TTL 缓存。键约定为 "pathid:filepath" 形式。
// 命中时由 lru 自动刷新访问顺序（真 LRU）；过期条目在 Get 时惰性删除。
type Cache struct {
	c        *lru.Cache[string, *entry]
	ttl      time.Duration
}

// New 创建缓存。ttl 为单条有效期，maxEntries 为容量上限（LRU 驱逐）。
func New(ttl time.Duration, maxEntries int) *Cache {
	c, err := lru.New[string, *entry](maxEntries)
	if err != nil {
		// maxEntries <= 0 才会出错，调用方应保证 > 0
		panic(err)
	}
	return &Cache{c: c, ttl: ttl}
}

// Get 取值。命中且未过期返回数据；过期则删除并视为未命中。
func (c *Cache) Get(key string) ([]byte, bool) {
	e, ok := c.c.Get(key)
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expireAt) {
		c.c.Remove(key)
		return nil, false
	}
	return e.data, true
}

// Set 写入。data 为原始字节，由调用方决定缓存的是文件字节还是渲染片段。
func (c *Cache) Set(key string, data []byte) {
	c.c.Add(key, &entry{
		data:     data,
		expireAt: time.Now().Add(c.ttl),
	})
}

// Invalidate 删除键前缀匹配 prefix 的所有条目（用于按 pathid 失效整站缓存）。
func (c *Cache) Invalidate(prefix string) {
	for _, k := range c.c.Keys() {
		if strings.HasPrefix(k, prefix) {
			c.c.Remove(k)
		}
	}
}

// Len 返回当前条目数（含可能已过期但未惰性清理的，仅供观测）。
func (c *Cache) Len() int {
	return c.c.Len()
}
