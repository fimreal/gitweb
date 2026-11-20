package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 构造结构完整、>= minBinarySize 的可执行文件映像：
// 头部字段 + 合法的段/load command 表 + 填充。只有头字段的假二进制
// 过不了 verifyBinary，测试的成功路径必须用这种完整 fixture。

func elfBinary(machine uint16) []byte {
	b := make([]byte, 4096)
	copy(b, []byte{0x7f, 'E', 'L', 'F'})
	b[4] = 2 // ELFCLASS64
	b[5] = 1 // little-endian
	binary.LittleEndian.PutUint16(b[18:20], machine)
	binary.LittleEndian.PutUint64(b[32:40], 64) // e_phoff
	binary.LittleEndian.PutUint16(b[54:56], 56) // e_phentsize
	binary.LittleEndian.PutUint16(b[56:58], 1)  // e_phnum
	ph := b[64:]
	binary.LittleEndian.PutUint32(ph[0:4], 1) // PT_LOAD
	binary.LittleEndian.PutUint64(ph[8:16], 0)
	binary.LittleEndian.PutUint64(ph[32:40], uint64(len(b))) // p_filesz
	return b
}

func machOBinary(cpu uint32) []byte {
	b := make([]byte, 4096)
	binary.LittleEndian.PutUint32(b[:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(b[4:8], cpu)
	binary.LittleEndian.PutUint32(b[16:20], 1)  // ncmds
	binary.LittleEndian.PutUint32(b[20:24], 72) // sizeofcmds
	lc := b[32:]
	binary.LittleEndian.PutUint32(lc[0:4], 0x19) // LC_SEGMENT_64
	binary.LittleEndian.PutUint32(lc[4:8], 72)
	return b
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	return tarGzMulti(t, [][2]any{{name, content}})
}

func tarGzMulti(t *testing.T, entries [][2]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		name, content := e[0].(string), e[1].([]byte)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer 模拟 GitHub API（TLS）：latest API + API asset 端点 +
// browser_download_url 兜底 + 可选校验和 sidecar 与 digest 字段。
// tag 固定带一个版本号，用于断言任何错误输出都不会把它泄漏出来。
type releaseServer struct {
	assetFile string
	archive   []byte
	checksum  string // 非空则作为 <asset>.sha256 asset 发布
	digest    string // 非空则作为 asset 的 digest 字段返回
	gotAuth   *[]string
}

func newReleaseServer(t *testing.T, rs releaseServer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	record := func(r *http.Request) {
		if rs.gotAuth != nil {
			*rs.gotAuth = append(*rs.gotAuth, r.Header.Get("Authorization"))
		}
	}
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		asset := map[string]string{
			"name":                 rs.assetFile,
			"url":                  srv.URL + "/repos/o/r/releases/assets/1",
			"browser_download_url": srv.URL + "/download/v9.9.9/" + rs.assetFile,
		}
		if rs.digest != "" {
			asset["digest"] = rs.digest
		}
		assets := []map[string]string{asset}
		if rs.checksum != "" {
			assets = append(assets, map[string]string{
				"name":                 rs.assetFile + ".sha256",
				"url":                  srv.URL + "/repos/o/r/releases/assets/2",
				"browser_download_url": srv.URL + "/download/v9.9.9/" + rs.assetFile + ".sha256",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v9.9.9", "assets": assets})
	})
	mux.HandleFunc("/repos/o/r/releases/assets/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if strings.HasSuffix(r.URL.Path, "/2") {
			w.Write([]byte(rs.checksum))
			return
		}
		w.Write(rs.archive)
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			w.Write([]byte(rs.checksum))
			return
		}
		w.Write(rs.archive)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func upgradeOpts(srv *httptest.Server, exe, goos, goarch string) Options {
	return Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: goos, GOARCH: goarch,
		ExecutablePath: exe, Client: srv.Client(),
	}
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

func assertNoVersionLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "v9.9.9") || strings.Contains(msg, "://") || strings.Contains(msg, "127.0.0.1") {
		t.Fatalf("error leaks version or URL: %v", err)
	}
}

func TestUpgradeReplacesBinary(t *testing.T) {
	bin := elfBinary(62)
	srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: tarGz(t, "gitweb", bin)})
	exe, dir := writeExe(t, "gitweb")

	if err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64")); err != nil {
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

func TestUpgradeDarwinArm64(t *testing.T) {
	bin := machOBinary(0x0100000c)
	srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-darwin-arm64.tar.gz", archive: tarGz(t, "gitweb", bin)})
	exe, _ := writeExe(t, "gitweb")
	if err := Upgrade(context.Background(), upgradeOpts(srv, exe, "darwin", "arm64")); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeRejectsHTTPSource(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	exe, _ := writeExe(t, "gitweb")
	err := Upgrade(context.Background(), Options{
		BaseURL: srv.URL, Repo: "o/r", GOOS: "linux", GOARCH: "amd64", ExecutablePath: exe,
	})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http source must be rejected, got: %v", err)
	}
	assertExeUntouched(t, exe)
}

func TestUpgradeNoRelease(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
	if err == nil {
		t.Fatal("expected error when no release exists")
	}
	assertNoVersionLeak(t, err)
	assertExeUntouched(t, exe)
}

func TestUpgradeMissingAsset(t *testing.T) {
	srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-freebsd-amd64.tar.gz"})
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
	if err == nil || !strings.Contains(err.Error(), "no asset for linux/amd64") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertNoVersionLeak(t, err)
	assertExeUntouched(t, exe)
}

// 校验失败的各种形态：坏 gzip、gzip 尾部截断/CRC 损坏、缺二进制条目、
// 重复条目、坏 magic、架构不符、结构不完整、头部溢出 —— 原文件都必须原样。
func TestUpgradeVerificationFailuresKeepOriginal(t *testing.T) {
	valid := tarGz(t, "gitweb", elfBinary(62))
	truncated := valid[:len(valid)-3]
	badCRC := append([]byte(nil), valid...)
	badCRC[len(badCRC)-5] ^= 0xff // gzip 尾部 CRC32 区域
	noLoad := elfBinary(62)
	binary.LittleEndian.PutUint32(noLoad[64:68], 2) // PT_DYNAMIC instead of PT_LOAD
	oobSegment := elfBinary(62)
	binary.LittleEndian.PutUint64(oobSegment[64+32:64+40], 1<<40) // p_filesz past EOF
	// 溢出复现：phoff + phnum*phentsize 无符号绕回后曾切片 panic。
	phoffOverflow := elfBinary(62)
	binary.LittleEndian.PutUint64(phoffOverflow[32:40], ^uint64(0)-15)
	segOverflow := elfBinary(62)
	binary.LittleEndian.PutUint64(segOverflow[64+8:64+16], ^uint64(0)-15) // p_offset
	binary.LittleEndian.PutUint64(segOverflow[64+32:64+40], 32)           // p_filesz，加法绕回

	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"corrupt gzip", []byte("this is not a gzip stream")},
		{"truncated gzip tail", truncated},
		{"corrupt gzip crc", badCRC},
		{"no binary entry", tarGz(t, "README.md", []byte("hi"))},
		{"duplicate binary entries", tarGzMulti(t, [][2]any{{"gitweb", elfBinary(62)}, {"gitweb", elfBinary(62)}})},
		{"bad magic", tarGz(t, "gitweb", bytes.Repeat([]byte("#!/bin/sh\n"), 512))},
		{"arch mismatch", tarGz(t, "gitweb", elfBinary(183))},
		{"header-only fake elf", tarGz(t, "gitweb", append(elfBinary(62)[:64], make([]byte, 4096-64)...))},
		{"elf without loadable segment", tarGz(t, "gitweb", noLoad)},
		{"elf segment past eof", tarGz(t, "gitweb", oobSegment)},
		{"elf phoff overflow", tarGz(t, "gitweb", phoffOverflow)},
		{"elf segment offset overflow", tarGz(t, "gitweb", segOverflow)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: tc.archive})
			exe, dir := writeExe(t, "gitweb")
			err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
			if err == nil {
				t.Fatal("expected verification error")
			}
			assertNoVersionLeak(t, err)
			assertExeUntouched(t, exe)
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("temp files left behind: %v", entries)
			}
		})
	}
}

func TestUpgradeChecksum(t *testing.T) {
	bin := elfBinary(62)
	archive := tarGz(t, "gitweb", bin)
	sum := sha256.Sum256(archive)
	good := hex.EncodeToString(sum[:]) + "  gitweb-linux-amd64.tar.gz\n"

	t.Run("valid checksum", func(t *testing.T) {
		srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: archive, checksum: good})
		exe, _ := writeExe(t, "gitweb")
		if err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64")); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct{ name, checksum string }{
		{"mismatch", strings.Repeat("0", 64)},
		{"malformed", "not-a-digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: archive, checksum: tc.checksum})
			exe, _ := writeExe(t, "gitweb")
			err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
			if err == nil {
				t.Fatal("expected checksum error")
			}
			assertNoVersionLeak(t, err)
			assertExeUntouched(t, exe)
		})
	}
}

// GitHub API 的 digest 字段：存在就强制比对，且优先于 .sha256 sidecar。
func TestUpgradeDigest(t *testing.T) {
	bin := elfBinary(62)
	archive := tarGz(t, "gitweb", bin)
	sum := sha256.Sum256(archive)
	good := "sha256:" + hex.EncodeToString(sum[:])

	t.Run("valid digest", func(t *testing.T) {
		srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: archive, digest: good})
		exe, _ := writeExe(t, "gitweb")
		if err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("digest takes precedence over sidecar", func(t *testing.T) {
		// sidecar 内容是坏的，但 digest 正确：应成功且不再请求 sidecar。
		var auth []string
		srv := newReleaseServer(t, releaseServer{
			assetFile: "gitweb-linux-amd64.tar.gz", archive: archive,
			digest: good, checksum: "garbage", gotAuth: &auth,
		})
		exe, _ := writeExe(t, "gitweb")
		opts := upgradeOpts(srv, exe, "linux", "amd64")
		opts.Token = "s3cret"
		if err := Upgrade(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if len(auth) != 2 { // latest API + asset，不含 sidecar
			t.Fatalf("sidecar should not be fetched when digest is present: %d requests", len(auth))
		}
	})
	for _, tc := range []struct{ name, digest string }{
		{"mismatch", "sha256:" + strings.Repeat("0", 64)},
		{"malformed algorithm", "md5:" + hex.EncodeToString(sum[:])},
		{"malformed hex", "sha256:xyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: archive, digest: tc.digest})
			exe, _ := writeExe(t, "gitweb")
			err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
			if err == nil {
				t.Fatal("expected digest error")
			}
			assertNoVersionLeak(t, err)
			assertExeUntouched(t, exe)
		})
	}
}

func TestUpgradeSendsTokenOnSameOriginRequests(t *testing.T) {
	var auth []string
	bin := elfBinary(62)
	srv := newReleaseServer(t, releaseServer{
		assetFile: "gitweb-linux-amd64.tar.gz", archive: tarGz(t, "gitweb", bin), gotAuth: &auth,
	})
	exe, _ := writeExe(t, "gitweb")

	opts := upgradeOpts(srv, exe, "linux", "amd64")
	opts.Token = "s3cret"
	if err := Upgrade(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(auth) != 2 || auth[0] != "Bearer s3cret" || auth[1] != "Bearer s3cret" {
		t.Fatalf("auth headers = %v", auth)
	}
}

// API url 缺失时兜底 browser_download_url（同源场景）。
func TestUpgradeBrowserURLFallback(t *testing.T) {
	bin := elfBinary(62)
	archive := tarGz(t, "gitweb", bin)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9",
			"assets": []map[string]string{{
				"name":                 "gitweb-linux-amd64.tar.gz",
				"browser_download_url": srv.URL + "/download/v9.9.9/asset",
			}},
		})
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	exe, _ := writeExe(t, "gitweb")
	if err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64")); err != nil {
		t.Fatal(err)
	}
}

// API 返回的 asset 地址指向第三方主机：请求前就拒绝，token 不出门。
func TestUpgradeForeignAssetURLRejected(t *testing.T) {
	var foreignHits int
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits++
		w.Write(tarGz(t, "gitweb", elfBinary(62)))
	}))
	t.Cleanup(foreign.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":"gitweb-linux-amd64.tar.gz","url":%q}]}`,
			foreign.URL+"/v9.9.9/asset.tar.gz")
	})
	api := httptest.NewTLSServer(mux)
	t.Cleanup(api.Close)
	exe, _ := writeExe(t, "gitweb")

	opts := upgradeOpts(api, exe, "linux", "amd64")
	opts.Token = "s3cret"
	err := Upgrade(context.Background(), opts)
	if err == nil {
		t.Fatal("expected cross-origin rejection")
	}
	assertNoVersionLeak(t, err)
	if foreignHits != 0 {
		t.Fatalf("foreign host was contacted %d times", foreignHits)
	}
	assertExeUntouched(t, exe)
}

// 重定向每一跳都校验：跨主机（白名单外）、同主机换端口、HTTPS 降级都必须
// 拒绝，且错误不泄漏目标 URL/tag；同源重定向正常跟随并保留凭据。
func TestUpgradeRedirectPolicy(t *testing.T) {
	bin := elfBinary(62)
	archive := tarGz(t, "gitweb", bin)

	// assetAPI 返回指向本服务器 /download/ 的 release JSON。
	assetAPI := func(srvURL func() string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{{
					"name": "gitweb-linux-amd64.tar.gz",
					"url":  srvURL() + "/download/v9.9.9/asset",
				}},
			})
		}
	}

	t.Run("same-origin redirect follows with token", func(t *testing.T) {
		var auth []string
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{{
					"name": "gitweb-linux-amd64.tar.gz",
					"url":  srv.URL + "/download/redirect/v9.9.9/asset",
				}},
			})
		})
		mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/download/redirect/") {
				http.Redirect(w, r, "/download/v9.9.9/asset", http.StatusFound)
				return
			}
			auth = append(auth, r.Header.Get("Authorization"))
			w.Write(archive)
		})
		srv = httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		exe, _ := writeExe(t, "gitweb")
		opts := upgradeOpts(srv, exe, "linux", "amd64")
		opts.Token = "s3cret"
		if err := Upgrade(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if len(auth) != 1 || auth[0] != "Bearer s3cret" {
			t.Fatalf("token lost on same-origin redirect: %v", auth)
		}
	})

	t.Run("cross-host redirect rejected", func(t *testing.T) {
		var foreignHits int
		foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			foreignHits++
			w.Write(archive)
		}))
		t.Cleanup(foreign.Close)
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/repos/o/r/releases/latest", assetAPI(func() string { return srv.URL }))
		mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, foreign.URL+"/v9.9.9/asset", http.StatusFound)
		})
		srv = httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		exe, _ := writeExe(t, "gitweb")
		opts := upgradeOpts(srv, exe, "linux", "amd64")
		opts.Token = "s3cret"
		err := Upgrade(context.Background(), opts)
		if err == nil {
			t.Fatal("expected cross-host redirect rejection")
		}
		assertNoVersionLeak(t, err)
		if foreignHits != 0 {
			t.Fatalf("token-bearing request reached foreign host %d times", foreignHits)
		}
		assertExeUntouched(t, exe)
	})

	t.Run("port change rejected", func(t *testing.T) {
		other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(archive)
		}))
		t.Cleanup(other.Close)
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/repos/o/r/releases/latest", assetAPI(func() string { return srv.URL }))
		mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
			// 同主机名、不同端口 —— 有效端口变化也算跨源。
			target := "https://" + other.Listener.Addr().String() + "/v9.9.9/asset"
			http.Redirect(w, r, target, http.StatusFound)
		})
		srv = httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		exe, _ := writeExe(t, "gitweb")
		err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
		if err == nil {
			t.Fatal("expected port-change redirect rejection")
		}
		assertNoVersionLeak(t, err)
		assertExeUntouched(t, exe)
	})

	t.Run("https downgrade rejected", func(t *testing.T) {
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(archive)
		}))
		t.Cleanup(plain.Close)
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/repos/o/r/releases/latest", assetAPI(func() string { return srv.URL }))
		mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+"/v9.9.9/asset", http.StatusFound)
		})
		srv = httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		exe, _ := writeExe(t, "gitweb")
		err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
		if err == nil {
			t.Fatal("expected https-downgrade rejection")
		}
		assertNoVersionLeak(t, err)
		assertExeUntouched(t, exe)
	})
}

func TestSameOrigin(t *testing.T) {
	base, _ := url.Parse("https://api.github.com")
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"https://api.github.com/a", true},
		{"https://api.github.com:443/a", true},
		{"https://sub.api.github.com/a", false},
		{"https://api.github.com.evil.com/a", false},
		{"https://evil.com/a", false},
		{"https://api.github.com:8443/a", false},
		{"http://api.github.com/a", false},
	} {
		u, _ := url.Parse(tc.target)
		if got := sameOrigin(base, u); got != tc.want {
			t.Errorf("sameOrigin(%s) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

// asset 链路白名单与重定向策略的单元验证：GitHub CDN 域名放行但剥离
// Authorization，白名单外/降级/非默认端口拒绝，跳数封顶。
func TestRedirectPolicyUnit(t *testing.T) {
	base, _ := url.Parse("https://api.github.com")
	policy := redirectPolicy(base)

	check := func(rawURL string, via int) (*http.Request, error) {
		req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		err := policy(req, make([]*http.Request, via))
		return req, err
	}

	t.Run("same-origin keeps token", func(t *testing.T) {
		req, err := check("https://api.github.com/repos/o/r/releases/assets/1", 0)
		if err != nil || req.Header.Get("Authorization") != "Bearer s3cret" {
			t.Fatalf("same-origin hop must keep token: %v", err)
		}
	})
	for _, host := range []string{
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
		"github-releases.githubusercontent.com",
		"github.com",
	} {
		t.Run("cdn host allowed without token: "+host, func(t *testing.T) {
			req, err := check("https://"+host+"/signed/path", 1)
			if err != nil {
				t.Fatalf("GitHub asset CDN host must be allowed: %v", err)
			}
			if req.Header.Get("Authorization") != "" {
				t.Fatal("token must be stripped on cross-origin hop")
			}
		})
	}
	for _, rawURL := range []string{
		"https://evil.com/asset",
		"https://objects.githubusercontent.com.evil.com/asset",
		"https://objects.githubusercontent.com:8443/asset",
		"http://objects.githubusercontent.com/asset",
	} {
		t.Run("rejected: "+rawURL, func(t *testing.T) {
			if _, err := check(rawURL, 1); !errors.Is(err, errCrossOrigin) {
				t.Fatalf("expected cross-origin rejection, got: %v", err)
			}
		})
	}
	t.Run("hop cap", func(t *testing.T) {
		if _, err := check("https://api.github.com/a", maxRedirects); !errors.Is(err, errTooManyRedirects) {
			t.Fatalf("expected redirect cap, got: %v", err)
		}
	})
}

// 网络错误不得经 url.Error 泄漏含 tag 的 URL：连接失败与下载中途断连都测。
func TestUpgradeNetworkErrorsOmitURL(t *testing.T) {
	bin := elfBinary(62)
	archive := tarGz(t, "gitweb", bin)

	t.Run("api connection failure", func(t *testing.T) {
		srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: archive})
		exe, _ := writeExe(t, "gitweb")
		opts := upgradeOpts(srv, exe, "linux", "amd64")
		srv.Close() // API 请求直接连接失败
		err := Upgrade(context.Background(), opts)
		if err == nil {
			t.Fatal("expected network error")
		}
		assertNoVersionLeak(t, err)
		assertExeUntouched(t, exe)
	})

	t.Run("asset connection reset mid-download", func(t *testing.T) {
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{{
					"name": "gitweb-linux-amd64.tar.gz",
					"url":  srv.URL + "/download/v9.9.9/asset",
				}},
			})
		})
		mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
			// 不应答直接断开：client.Do 返回的 url.Error 携带完整 asset URL（含 tag）。
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
		})
		srv = httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		exe, _ := writeExe(t, "gitweb")
		err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
		if err == nil {
			t.Fatal("expected network error")
		}
		assertNoVersionLeak(t, err)
		assertExeUntouched(t, exe)
	})
}

// 同源重定向循环：自定义 CheckRedirect 覆盖了 net/http 默认跳数上限，
// 必须自己封顶，否则会一直转到客户端超时。
func TestUpgradeRedirectLoop(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9",
			"assets": []map[string]string{{
				"name": "gitweb-linux-amd64.tar.gz",
				"url":  srv.URL + "/download/v9.9.9/loop",
			}},
		})
	})
	hops := 0
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/download/v9.9.9/loop", http.StatusFound)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
	if err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("expected redirect cap error, got: %v", err)
	}
	if hops > maxRedirects+1 {
		t.Fatalf("redirect cap not enforced: %d hops", hops)
	}
	assertNoVersionLeak(t, err)
	assertExeUntouched(t, exe)
}

// gzip 炸弹：压缩体积很小、解压后远超预算的归档必须被解压总量上限挡住。
func TestUpgradeGzipBomb(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "junk", Mode: 0o644, Size: 300 << 20, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 1<<20)
	for i := 0; i < 300; i++ {
		if _, err := tw.Write(zeros); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: buf.Bytes()})
	exe, _ := writeExe(t, "gitweb")
	err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
	if err == nil || !strings.Contains(err.Error(), "decompressed size limit") {
		t.Fatalf("expected decompressed size limit error, got: %v", err)
	}
	assertNoVersionLeak(t, err)
	assertExeUntouched(t, exe)
}

// 声明了 <asset>.sha256 却没有下载地址：发布异常，必须报错而不是当作无校验和。
func TestUpgradeChecksumAssetWithoutURL(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9",
			"assets": []map[string]string{
				{"name": "gitweb-linux-amd64.tar.gz", "url": srv.URL + "/download/v9.9.9/asset"},
				{"name": "gitweb-linux-amd64.tar.gz.sha256", "url": "", "browser_download_url": ""},
			},
		})
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	exe, _ := writeExe(t, "gitweb")

	err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
	if err == nil || !strings.Contains(err.Error(), "no download URL") {
		t.Fatalf("expected checksum-without-URL error, got: %v", err)
	}
	assertNoVersionLeak(t, err)
	assertExeUntouched(t, exe)
}

// 安装目录含版本号时，文件系统错误不得经 PathError 把目录名/tag 带进日志。
func TestUpgradeFSErrorsOmitVersionedPath(t *testing.T) {
	bin := elfBinary(62)
	srv := newReleaseServer(t, releaseServer{assetFile: "gitweb-linux-amd64.tar.gz", archive: tarGz(t, "gitweb", bin)})
	realRename := os.Rename
	t.Cleanup(func() { renameFn = realRename })

	versionedExe := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "gitweb-v9.9.9")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(dir, "gitweb")
		if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		return exe
	}
	assertNoPathLeak := func(t *testing.T, err error, exe string) {
		t.Helper()
		if err == nil {
			t.Fatal("expected error")
		}
		msg := err.Error()
		if strings.Contains(msg, exe) || strings.Contains(msg, filepath.Dir(exe)) || strings.Contains(msg, "gitweb-v9.9.9") {
			t.Fatalf("error leaks versioned install path: %v", err)
		}
		assertNoVersionLeak(t, err)
	}

	t.Run("stat failure", func(t *testing.T) {
		exe := filepath.Join(filepath.Dir(versionedExe(t)), "missing")
		err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
		assertNoPathLeak(t, err, exe)
	})

	t.Run("rename failure", func(t *testing.T) {
		exe := versionedExe(t)
		renameFn = func(old, new string) error { return errors.New("injected") }
		err := Upgrade(context.Background(), upgradeOpts(srv, exe, "linux", "amd64"))
		assertNoPathLeak(t, err, exe)
		assertExeUntouched(t, exe)
		entries, _ := os.ReadDir(filepath.Dir(exe))
		if len(entries) != 1 {
			t.Fatalf("temp files left behind: %v", entries)
		}
	})
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
		{"linux", "amd64", elfBinary(62)[:64], false}, // 只有头字段
		{"darwin", "amd64", machOBinary(0x01000007), true},
		{"darwin", "arm64", machOBinary(0x0100000c), true},
		{"darwin", "arm64", machOBinary(0x01000007), false},
		{"windows", "amd64", elfBinary(62), false}, // Windows 已不支持
		{"plan9", "amd64", elfBinary(62), false},
	} {
		err := verifyBinary(tc.bin, tc.goos, tc.goarch)
		if (err == nil) != tc.ok {
			t.Errorf("verifyBinary(%s/%s) = %v, want ok=%v", tc.goos, tc.goarch, err, tc.ok)
		}
	}
}
