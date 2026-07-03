package cache

import (
	"testing"
	"time"
)

func TestCacheSetGet(t *testing.T) {
	c := New(time.Minute, 16)
	c.Set("a:b", []byte("hello"))
	got, ok := c.Get("a:b")
	if !ok {
		t.Fatal("miss after set")
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want hello", got)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("hit on missing key")
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	c := New(50*time.Millisecond, 16)
	c.Set("k", []byte("v"))
	if _, ok := c.Get("k"); !ok {
		t.Fatal("miss before TTL")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("hit after TTL expiry")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := New(time.Minute, 3)
	c.Set("k1", []byte("1"))
	c.Set("k2", []byte("2"))
	c.Set("k3", []byte("3"))
	// 访问 k1，使其成为最近使用，k2 成为最久未用
	c.Get("k1")
	// 插入 k4，应驱逐 k2
	c.Set("k4", []byte("4"))
	if _, ok := c.Get("k2"); ok {
		t.Error("k2 should have been evicted")
	}
	if _, ok := c.Get("k1"); !ok {
		t.Error("k1 should survive (recently accessed)")
	}
	if _, ok := c.Get("k4"); !ok {
		t.Error("k4 should be present")
	}
}

func TestCacheInvalidatePrefix(t *testing.T) {
	c := New(time.Minute, 16)
	c.Set("site1:a", []byte("a"))
	c.Set("site1:b", []byte("b"))
	c.Set("site2:c", []byte("c"))
	c.Invalidate("site1:")
	if _, ok := c.Get("site1:a"); ok {
		t.Error("site1:a should be invalidated")
	}
	if _, ok := c.Get("site1:b"); ok {
		t.Error("site1:b should be invalidated")
	}
	if _, ok := c.Get("site2:c"); !ok {
		t.Error("site2:c should survive")
	}
}

func TestCacheInvalidateNoPrefixMatch(t *testing.T) {
	c := New(time.Minute, 16)
	c.Set("foo:bar", []byte("x"))
	c.Invalidate("nomatch")
	if _, ok := c.Get("foo:bar"); !ok {
		t.Error("foo:bar should survive non-matching invalidate")
	}
}
