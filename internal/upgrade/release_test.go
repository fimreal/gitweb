package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.6.1", "v0.6.2", -1, true},
		{"v0.7.0", "v0.6.9", 1, true},
		{"v1.0.0", "v1.0.0", 0, true},
		{"0.7.0", "v0.7.0", 0, true},
		{"v1.0.0-rc1", "v1.0.0", -1, true},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", -1, true}, // 点分数字段按数值比
		{"v1.0.0-rc2", "v1.0.0-rc10", 1, true},    // 非点分后缀按语义化版本的字典序
		{"v1.0.0-beta", "v1.0.0-alpha", 1, true},
		{"v1.2.3+build.5", "v1.2.3", 0, true},
		{"dev", "v1.0.0", 0, false},
		{"v1", "v1.0.0", 0, false},
		{"", "v1.0.0", 0, false},
	}
	for _, tc := range cases {
		got, ok := CompareVersions(tc.a, tc.b)
		if ok != tc.ok || got != tc.want {
			t.Errorf("CompareVersions(%q,%q) = (%d,%v), want (%d,%v)", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

// taggedReleaseServer 提供按 tag 查询的 release 元数据，并记录请求过的路径。
func taggedReleaseServer(t *testing.T, tag string, bin []byte, seen *[]string) *httptest.Server {
	t.Helper()
	archive := tarGz(t, "gitweb", bin)
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, "/"+tag) {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []map[string]string{{
				"name": "gitweb-linux-amd64.tar.gz",
				"url":  srv.URL + "/repos/o/r/releases/assets/1",
			}},
		})
	})
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveAndInstallByTag(t *testing.T) {
	bin := elfBinary(62)
	var seen []string
	srv := taggedReleaseServer(t, "v0.6.1", bin, &seen)
	exe, _ := writeExe(t, "gitweb")

	opts := upgradeOpts(srv, exe, "linux", "amd64")
	opts.Tag = "v0.6.1"

	rel, err := Resolve(context.Background(), opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rel.Tag != "v0.6.1" {
		t.Fatalf("tag = %q, want v0.6.1", rel.Tag)
	}
	// Resolve 不应写盘
	assertExeUntouched(t, exe)

	if err := Install(context.Background(), opts, rel); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != string(bin) {
		t.Fatalf("binary not replaced from tagged release: %v", err)
	}
	if len(seen) == 0 || !strings.HasSuffix(seen[0], "/releases/tags/v0.6.1") {
		t.Fatalf("expected tagged endpoint, saw %v", seen)
	}
}

func TestResolveUnknownTag(t *testing.T) {
	var seen []string
	srv := taggedReleaseServer(t, "v0.6.1", elfBinary(62), &seen)
	opts := upgradeOpts(srv, filepath.Join(t.TempDir(), "gitweb"), "linux", "amd64")
	opts.Tag = "v9.9.9"

	_, err := Resolve(context.Background(), opts)
	if !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("err = %v, want ErrReleaseNotFound", err)
	}
}

func TestResolveRejectsMalformedTag(t *testing.T) {
	var seen []string
	srv := taggedReleaseServer(t, "v0.6.1", elfBinary(62), &seen)
	opts := upgradeOpts(srv, filepath.Join(t.TempDir(), "gitweb"), "linux", "amd64")
	opts.Tag = "../../etc/passwd"

	if _, err := Resolve(context.Background(), opts); err == nil {
		t.Fatal("want error for malformed tag, got nil")
	}
	if len(seen) != 0 {
		t.Fatalf("malformed tag must not reach the network, saw %v", seen)
	}
}
