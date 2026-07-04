package registry

import (
	"strings"
	"testing"
)

func TestRegisterCustomPathID(t *testing.T) {
	r := New()
	site, err := r.Register("https://github.com/o/repo", "myid", "main", "github", nil, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if site.PathID != "myid" {
		t.Errorf("PathID = %q, want myid", site.PathID)
	}
	if site.Ref != "main" {
		t.Errorf("Ref = %q, want main", site.Ref)
	}
}

func TestRegisterDuplicatePathID(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/repo", "dup", "main", "github", nil, false); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := r.Register("https://github.com/o/other", "dup", "main", "github", nil, false)
	if err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected duplicate message, got %v", err)
	}
}

func TestRegisterAutoPathID(t *testing.T) {
	r := New()
	site, err := r.Register("https://github.com/o/repo", "", "main", "github", nil, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(site.PathID) != 8 {
		t.Errorf("auto PathID length = %d, want 8", len(site.PathID))
	}
	if !IsValidPathID(site.PathID) {
		t.Errorf("auto PathID %q failed validation", site.PathID)
	}
}

func TestRegisterInvalidPathID(t *testing.T) {
	r := New()
	for _, id := range []string{"with space", "has/slash", "ab cd", "中文"} {
		if _, err := r.Register("https://github.com/o/repo", id, "main", "github", nil, false); err == nil {
			t.Errorf("expected error for invalid pathid %q, got nil", id)
		}
	}
}

func TestRegisterReservedPathID(t *testing.T) {
	r := New()
	for _, id := range []string{"api", "static", "healthz"} {
		if _, err := r.Register("https://github.com/o/repo", id, "main", "github", nil, false); err == nil {
			t.Errorf("expected error for reserved pathid %q, got nil", id)
		}
	}
}

func TestRegisterRefDefault(t *testing.T) {
	r := New()
	site, err := r.Register("https://github.com/o/repo", "x", "", "github", nil, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if site.Ref != "main" {
		t.Errorf("default Ref = %q, want main", site.Ref)
	}
}

func TestGetNotFound(t *testing.T) {
	r := New()
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("expected not found, got nil")
	}
}

func TestRemove(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/repo", "rm", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("rm"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := r.Get("rm"); err == nil {
		t.Fatal("expected not found after remove")
	}
	if err := r.Remove("rm"); err == nil {
		t.Fatal("expected error removing non-existent")
	}
}

func TestIsValidPathID(t *testing.T) {
	good := []string{"a", "ab12", "my_id", "my-id", "AaBb09_-", strings.Repeat("a", 32)}
	bad := []string{"", "a b", "a/b", "a.b", "中文", strings.Repeat("a", 33)}
	for _, id := range good {
		if !IsValidPathID(id) {
			t.Errorf("IsValidPathID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if IsValidPathID(id) {
			t.Errorf("IsValidPathID(%q) = true, want false", id)
		}
	}
}

func TestHiddenNotInListPublic(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/pub", "pub", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("https://github.com/o/secret", "secret", "main", "github", nil, true); err != nil {
		t.Fatal(err)
	}
	pub := r.ListPublic()
	if len(pub) != 1 {
		t.Fatalf("ListPublic = %d sites, want 1", len(pub))
	}
	if pub[0].PathID != "pub" {
		t.Errorf("ListPublic pathid = %q, want pub", pub[0].PathID)
	}
	if got := len(r.List()); got != 2 {
		t.Errorf("List = %d sites, want 2", got)
	}
	// 隐藏站点仍可直链 Get
	if _, err := r.Get("secret"); err != nil {
		t.Errorf("Get(secret): %v", err)
	}
}
