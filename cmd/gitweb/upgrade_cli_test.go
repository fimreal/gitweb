package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fimreal/gitweb/internal/upgrade"
)

// fakeELF 造一个能过 upgrade 结构校验的 linux/amd64 ELF（头 + 一个 PT_LOAD）。
func fakeELF() []byte {
	const size = 8192
	b := make([]byte, size)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1})
	b[18], b[19] = 62, 0 // e_machine = EM_X86_64
	b[32] = 64           // e_phoff
	b[54], b[55] = 56, 0 // e_phentsize
	b[56], b[57] = 1, 0  // e_phnum
	b[64] = 1            // p_type = PT_LOAD
	b[72] = 0            // p_offset
	b[96] = 0            // p_filesz
	return b
}

func fakeArchive(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "gitweb", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bin); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// cliReleaseServer 模拟更新源：latest 固定 v9.9.9，另可按 tag 提供 v1.2.3。
type cliReleaseServer struct {
	archive     []byte
	checksum    string // 非空则附带 <asset>.sha256
	hits        atomic.Int64
	tagRequests atomic.Int64
}

func newCLIReleaseServer(t *testing.T, s *cliReleaseServer) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	asset := func(w http.ResponseWriter, tag string) {
		s.hits.Add(1)
		entry := map[string]string{
			"name": "gitweb-linux-amd64.tar.gz",
			"url":  srv.URL + "/repos/o/r/releases/assets/1",
		}
		assets := []map[string]string{entry}
		if s.checksum != "" {
			assets = append(assets, map[string]string{
				"name": "gitweb-linux-amd64.tar.gz.sha256",
				"url":  srv.URL + "/repos/o/r/releases/assets/2",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) { asset(w, "v9.9.9") })
	mux.HandleFunc("/repos/o/r/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		s.tagRequests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/v9.9.9"):
			asset(w, "v9.9.9")
		case strings.HasSuffix(r.URL.Path, "/v1.2.3"):
			asset(w, "v1.2.3")
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/2") {
			w.Write([]byte(s.checksum))
			return
		}
		w.Write(s.archive)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newCLI 组装一个指向假更新源的 upgradeCommand，exe 为待替换的旧二进制。
func newCLI(t *testing.T, srv *httptest.Server, exe, current string, isTTY bool, stdin string) upgradeCommand {
	t.Helper()
	return upgradeCommand{
		opts: upgrade.Options{
			BaseURL:        srv.URL,
			Repo:           "o/r",
			GOOS:           "linux",
			GOARCH:         "amd64",
			ExecutablePath: exe,
			Client:         srv.Client(),
		},
		stdin:   strings.NewReader(stdin),
		stdout:  &bytes.Buffer{},
		stderr:  &bytes.Buffer{},
		isTTY:   isTTY,
		version: current,
	}
}

func (c upgradeCommand) out() string  { return c.stdout.(*bytes.Buffer).String() }
func (c upgradeCommand) errs() string { return c.stderr.(*bytes.Buffer).String() }

func writeOldExe(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "gitweb")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func assertOldExe(t *testing.T, exe string) {
	t.Helper()
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "old binary" {
		t.Fatalf("原二进制被改动了: %q, %v", got, err)
	}
}

// TestUpgradeCLIConfirmDeclined：确认时答 n → 不下载不替换。
func TestUpgradeCLIConfirmDeclined(t *testing.T) {
	bin := fakeELF()
	s := &cliReleaseServer{archive: fakeArchive(t, bin)}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v0.6.2", true, "n\n")

	if code := c.run(nil); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertOldExe(t, exe)
	out := c.out()
	for _, want := range []string{"Current version: v0.6.2", "Target version:  v9.9.9", "Proceed with upgrade? [y/N]"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q\n%s", want, out)
		}
	}
	if !strings.Contains(c.out(), "Upgrade cancelled.") {
		t.Errorf("应提示已取消: %s", c.out())
	}
}

// TestUpgradeCLIYesSkipsPrompt：--yes 不问直接装。
func TestUpgradeCLIYesSkipsPrompt(t *testing.T) {
	bin := fakeELF()
	s := &cliReleaseServer{archive: fakeArchive(t, bin)}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v0.6.2", false, "")

	if code := c.run([]string{"--yes"}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, c.errs())
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, bin) {
		t.Fatal("二进制未被替换")
	}
	if strings.Contains(c.out(), "Proceed with") {
		t.Errorf("--yes 不该出现确认提示: %s", c.out())
	}
	if !strings.Contains(c.out(), "Done: v0.6.2 -> v9.9.9") {
		t.Errorf("成功摘要缺版本号: %s", c.out())
	}
}

// TestUpgradeCLIVersionFlag：--version 走 tags 端点。
func TestUpgradeCLIVersionFlag(t *testing.T) {
	bin := fakeELF()
	s := &cliReleaseServer{archive: fakeArchive(t, bin)}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v0.6.2", false, "")

	if code := c.run([]string{"--yes", "--version", "v1.2.3"}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, c.errs())
	}
	if s.tagRequests.Load() == 0 {
		t.Fatal("没有请求 tags 端点")
	}
	if !strings.Contains(c.out(), "Done: v0.6.2 -> v1.2.3") {
		t.Errorf("目标版本不对: %s", c.out())
	}
}

// TestUpgradeCLIDowngradeNeedsExplicitVersion：latest 比当前旧 → 拒绝，除非显式 --version。
func TestUpgradeCLIDowngradeNeedsExplicitVersion(t *testing.T) {
	s := &cliReleaseServer{archive: fakeArchive(t, fakeELF())}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v10.0.0", false, "")

	if code := c.run([]string{"--yes"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertOldExe(t, exe)
	if !strings.Contains(c.errs(), "--version") {
		t.Errorf("拒绝时必须提示用 --version 显式降级: %s", c.errs())
	}
}

// TestUpgradeCLIDowngradeExplicit：显式 --version 指向旧版 → 允许，提示语写明降级。
func TestUpgradeCLIDowngradeExplicit(t *testing.T) {
	bin := fakeELF()
	s := &cliReleaseServer{archive: fakeArchive(t, bin)}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v10.0.0", true, "y\n")

	if code := c.run([]string{"--version", "v1.2.3"}); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, c.errs())
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, bin) {
		t.Fatal("降级没有替换二进制")
	}
	out := c.out()
	if !strings.Contains(out, "Target version:  v1.2.3 (downgrade)") || !strings.Contains(out, "Proceed with downgrade?") {
		t.Errorf("降级提示不明确: %s", out)
	}
	if !strings.Contains(out, "Downgrading to v1.2.3") {
		t.Errorf("执行阶段应写明是降级: %s", out)
	}
}

// TestUpgradeCLIUnknownVersion：指定版本不存在 → 404 分类，回显用户输入的 tag。
func TestUpgradeCLIUnknownVersion(t *testing.T) {
	s := &cliReleaseServer{archive: fakeArchive(t, fakeELF())}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v0.6.2", false, "")

	if code := c.run([]string{"--yes", "--version", "v0.0.1"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertOldExe(t, exe)
	if !strings.Contains(c.errs(), "v0.0.1") {
		t.Errorf("应回显请求的版本: %s", c.errs())
	}
}

// TestUpgradeCLIChecksumMismatch：校验和不符 → 不落盘。
func TestUpgradeCLIChecksumMismatch(t *testing.T) {
	bin := fakeELF()
	sum := sha256.Sum256(bin)
	bad := strings.Repeat("0", 32) + hex.EncodeToString(sum[:16]) // 长度对但内容错
	s := &cliReleaseServer{archive: fakeArchive(t, bin), checksum: bad + "  gitweb-linux-amd64.tar.gz\n"}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v0.6.2", false, "")

	if code := c.run([]string{"--yes"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertOldExe(t, exe)
	if !strings.Contains(c.errs(), "checksum mismatch") {
		t.Errorf("错误分类不对: %s", c.errs())
	}
}

// TestUpgradeCLINonTTYRefuses：非 TTY 且没带 --yes → 直接拒绝，连请求都不发。
func TestUpgradeCLINonTTYRefuses(t *testing.T) {
	s := &cliReleaseServer{archive: fakeArchive(t, fakeELF())}
	srv := newCLIReleaseServer(t, s)
	exe := writeOldExe(t)
	c := newCLI(t, srv, exe, "v0.6.2", false, "")

	if code := c.run(nil); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertOldExe(t, exe)
	if s.hits.Load() != 0 {
		t.Errorf("非交互拒绝时不该发请求，实际 %d 次", s.hits.Load())
	}
	if !strings.Contains(c.errs(), "--yes") {
		t.Errorf("拒绝时要说明怎么继续: %s", c.errs())
	}
}

// TestCurrentVersionFallback：编译期没注入版本时不能硬编码成某个具体版本。
func TestCurrentVersionFallback(t *testing.T) {
	orig := version
	defer func() { version = orig }()

	version = "v1.4.2"
	if got := currentVersion(); got != "v1.4.2" {
		t.Errorf("注入版本时应原样返回: %s", got)
	}
	version = "dev"
	got := currentVersion()
	if got != "dev" && !strings.HasPrefix(got, "v") {
		t.Errorf("未注入版本时应退回 build info 或 dev，实际 %q", got)
	}
}
