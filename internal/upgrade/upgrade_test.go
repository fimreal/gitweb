package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 各平台的合法二进制头，只含 verifyBinary 需要的字段。
func elfBinary(machine uint16) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2 // ELFCLASS64
	b[5] = 1 // little-endian
	binary.LittleEndian.PutUint16(b[18:20], machine)
	return b
}

func peBinary(machine uint16) []byte {
	b := make([]byte, 0x46)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:0x40], 0x40)
	copy(b[0x40:], []byte{'P', 'E', 0, 0})
	binary.LittleEndian.PutUint16(b[0x44:0x46], machine)
	return b
}

func machOBinary(cpu uint32) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b[:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(b[4:8], cpu)
	return b
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer 模拟 Gitea：latest API + asset 下载。tag 固定带一个版本号，
// 用于断言任何错误输出都不会把它泄漏出来。
func releaseServer(t *testing.T, assetFile string, archive []byte, gotAuth *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/api/v1/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = append(*gotAuth, r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9",
			"assets": []map[string]string{{
				"name":                 assetFile,
				"browser_download_url": srv.URL + "/download/" + assetFile,
			}},
		})
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = append(*gotAuth, r.Header.Get("Authorization"))
		}
		w.Write(archive)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeExe 在独立目录里放一个旧二进制，返回其路径与所在目录。
func writeExe(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, name)
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, dir
}

func assertExeUntouched(t *testing.T, exe string) {
	t.Helper()
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "old binary" {
		t.Fatalf("original executable was modified: %q, %v", got, err)
	}
}

func TestUpgradeReplacesBinary(t *testing.T) {
	bin := elfBinary(62)
	srv := releaseServer(t, "gitweb-linux-amd64.tar.gz", tarGz(t, "gitweb", bin), nil)
	exe, dir := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("executable not replaced: %v", err)
	}
	info, err := os.Stat(exe)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("mode not preserved: %v, %v", info, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// Windows 路径：旧文件先 rename 成 .old 再让位，结束后 .old 应被清理。
// 在 Linux 宿主上跑同一套 rename 逻辑即可验证顺序与清理。
func TestUpgradeWindowsRenameDance(t *testing.T) {
	bin := peBinary(0x8664)
	srv := releaseServer(t, "gitweb-windows-amd64.tar.gz", tarGz(t, "gitweb.exe", bin), nil)
	exe, dir := writeExe(t, "gitweb.exe")
	// 上次升级残留的 .old，本次应顺带清掉。
	if err := os.WriteFile(exe+".old", []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: "windows", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, bin) {
		t.Fatal("executable not replaced")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "gitweb.exe" {
		t.Fatalf("unexpected dir contents: %v", entries)
	}
}

func TestUpgradeDarwinArm64(t *testing.T) {
	bin := machOBinary(0x0100000c)
	srv := releaseServer(t, "gitweb-darwin-arm64.tar.gz", tarGz(t, "gitweb", bin), nil)
	exe, _ := writeExe(t, "gitweb")
	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: "darwin", GOARCH: "arm64", ExecutablePath: exe,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeNoRelease(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err == nil {
		t.Fatal("expected error when no release exists")
	}
	assertExeUntouched(t, exe)
}

func TestUpgradeMissingAsset(t *testing.T) {
	srv := releaseServer(t, "gitweb-freebsd-amd64.tar.gz", nil, nil)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err == nil || !strings.Contains(err.Error(), "no asset for linux/amd64") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "v9.9.9") {
		t.Fatalf("error leaks version: %v", err)
	}
	assertExeUntouched(t, exe)
}

// 校验失败的三种形态：坏 gzip、缺二进制条目、magic/架构不符 —— 原文件都必须原样。
func TestUpgradeVerificationFailuresKeepOriginal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"corrupt gzip", []byte("this is not a gzip stream")},
		{"no binary entry", tarGz(t, "README.md", []byte("hi"))},
		{"bad magic", tarGz(t, "gitweb", []byte("#!/bin/sh\necho pwned"))},
		{"arch mismatch", tarGz(t, "gitweb", elfBinary(183))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := releaseServer(t, "gitweb-linux-amd64.tar.gz", tc.archive, nil)
			exe, dir := writeExe(t, "gitweb")
			err := Upgrade(context.Background(), Options{
				BaseURL: srv.URL, Repo: "o/r", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
			})
			if err == nil {
				t.Fatal("expected verification error")
			}
			if strings.Contains(err.Error(), "v9.9.9") {
				t.Fatalf("error leaks version: %v", err)
			}
			assertExeUntouched(t, exe)
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("temp files left behind: %v", entries)
			}
		})
	}
}

func TestUpgradeSendsTokenToSameHostOnly(t *testing.T) {
	var auth []string
	bin := elfBinary(62)
	srv := releaseServer(t, "gitweb-linux-amd64.tar.gz", tarGz(t, "gitweb", bin), &auth)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", Token: "s3cret", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auth) != 2 || auth[0] != "token s3cret" || auth[1] != "token s3cret" {
		t.Fatalf("auth headers = %v", auth)
	}
}

// asset 地址指向第三方主机时不得携带 token。
func TestUpgradeTokenNotSentToForeignHost(t *testing.T) {
	var foreignAuth []string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignAuth = append(foreignAuth, r.Header.Get("Authorization"))
		w.Write(tarGz(t, "gitweb", elfBinary(62)))
	}))
	t.Cleanup(foreign.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"gitweb-linux-amd64.tar.gz","browser_download_url":%q}]}`,
			foreign.URL+"/asset.tar.gz")
	})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), Options{
		BaseURL: api.URL, Repo: "o/r", Token: "s3cret", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(foreignAuth) != 1 || foreignAuth[0] != "" {
		t.Fatalf("token leaked to foreign host: %v", foreignAuth)
	}
}

func TestVerifyBinary(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
		bin          []byte
		ok           bool
	}{
		{"linux", "amd64", elfBinary(62), true},
		{"linux", "arm64", elfBinary(183), true},
		{"linux", "amd64", elfBinary(183), false},
		{"linux", "amd64", elfBinary(62)[:10], false},
		{"windows", "amd64", peBinary(0x8664), true},
		{"windows", "arm64", peBinary(0xaa64), true},
		{"windows", "amd64", peBinary(0xaa64), false},
		{"darwin", "amd64", machOBinary(0x01000007), true},
		{"darwin", "arm64", machOBinary(0x0100000c), true},
		{"darwin", "arm64", machOBinary(0x01000007), false},
		{"plan9", "amd64", elfBinary(62), false},
	} {
		err := verifyBinary(tc.bin, tc.goos, tc.goarch)
		if (err == nil) != tc.ok {
			t.Errorf("verifyBinary(%s/%s) = %v, want ok=%v", tc.goos, tc.goarch, err, tc.ok)
		}
	}
}
