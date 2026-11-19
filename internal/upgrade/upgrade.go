// Package upgrade 实现自更新：从源仓库的 Gitea release 下载最新二进制并替换当前可执行文件。
//
// 发布产物的唯一来源是 git.epurs.com 上的 fimreal/gitweb（Gitea 实例，非 github.com）：
// release API 为 /api/v1/repos/<owner>/<repo>/releases/latest，asset 命名与
// .github/workflows/release.yml 一致：gitweb-<os>-<arch>.tar.gz，包内仅一个
// gitweb（Windows 为 gitweb.exe）。下载地址直接取 API 返回的 browser_download_url，
// 不自行拼接路径。
//
// 按需求约定，本包任何错误信息与日志都不得包含版本号/tag 名。
package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// DefaultBaseURL 是发布仓库所在的 Gitea 实例根地址。
	DefaultBaseURL = "http://git.epurs.com"
	// DefaultRepo 是发布产物的源仓库（owner/name）。
	DefaultRepo = "fimreal/gitweb"

	// maxDownloadSize 限制单个 asset 的下载体积，防止异常响应耗尽内存。
	maxDownloadSize = 256 << 20
)

// Options 控制一次自更新。零值字段取默认；GOOS/GOARCH/ExecutablePath 供测试注入。
type Options struct {
	BaseURL        string // Gitea 实例根地址，默认 DefaultBaseURL
	Repo           string // owner/name，默认 DefaultRepo
	Token          string // Gitea access token；私有仓库必填，公开仓库可留空
	GOOS           string // 目标平台，默认 runtime.GOOS
	GOARCH         string // 目标架构，默认 runtime.GOARCH
	ExecutablePath string // 要替换的可执行文件路径，默认当前进程
	Client         *http.Client
}

// Upgrade 下载最新 release 的二进制并替换当前可执行文件。
// 下载、解包、校验的任何一步失败都直接返回错误，原文件保持不动。
func Upgrade(ctx context.Context, opts Options) error {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 5 * time.Minute}
	}

	assetURL, err := latestAssetURL(ctx, opts)
	if err != nil {
		return err
	}
	archive, err := download(ctx, opts, assetURL)
	if err != nil {
		return err
	}
	bin, err := extractBinary(archive, opts.GOOS)
	if err != nil {
		return err
	}
	if err := verifyBinary(bin, opts.GOOS, opts.GOARCH); err != nil {
		return fmt.Errorf("downloaded binary failed verification: %w", err)
	}

	exePath := opts.ExecutablePath
	if exePath == "" {
		exePath, err = os.Executable()
		if err != nil {
			return fmt.Errorf("locate current executable: %w", err)
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			return fmt.Errorf("resolve executable path: %w", err)
		}
	}
	return replaceBinary(exePath, bin, opts.GOOS)
}

// assetName 与 release.yml 的打包命名一一对应。
func assetName(goos, goarch string) string {
	return fmt.Sprintf("gitweb-%s-%s.tar.gz", goos, goarch)
}

func binaryName(goos string) string {
	if goos == "windows" {
		return "gitweb.exe"
	}
	return "gitweb"
}

// authedGet 发起 GET；token 只附加给与 BaseURL 同主机的请求，
// 避免 asset 下载重定向到第三方存储时把凭据带出去。
func authedGet(ctx context.Context, opts Options, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if opts.Token != "" {
		if base, err := url.Parse(opts.BaseURL); err == nil {
			if target, err := url.Parse(rawURL); err == nil && target.Host == base.Host {
				req.Header.Set("Authorization", "token "+opts.Token)
			}
		}
	}
	return opts.Client.Do(req)
}

// latestAssetURL 查询最新 release 并选出当前平台的 asset 下载地址。
// 刻意不解析、不返回 tag 名 —— 版本号不允许出现在任何输出里。
func latestAssetURL(ctx context.Context, opts Options) (string, error) {
	api := fmt.Sprintf("%s/api/v1/repos/%s/releases/latest", opts.BaseURL, opts.Repo)
	resp, err := authedGet(ctx, opts, api)
	if err != nil {
		return "", fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errors.New("no published release found on the update server")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release API returned %s", resp.Status)
	}
	var rel struct {
		Assets []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("parse release response: %w", err)
	}
	want := assetName(opts.GOOS, opts.GOARCH)
	for _, a := range rel.Assets {
		if a.Name == want && a.BrowserDownloadURL != "" {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("latest release has no asset for %s/%s", opts.GOOS, opts.GOARCH)
}

func download(ctx context.Context, opts Options, assetURL string) ([]byte, error) {
	resp, err := authedGet(ctx, opts, assetURL)
	if err != nil {
		return nil, fmt.Errorf("download release asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asset download returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, fmt.Errorf("read release asset: %w", err)
	}
	if len(data) > maxDownloadSize {
		return nil, errors.New("release asset exceeds size limit")
	}
	return data, nil
}

// extractBinary 从 tar.gz 中解出唯一的二进制条目。只按条目基名匹配、
// 内容写入调用方指定的路径，归档里的路径字段不参与落盘，天然免疫路径穿越。
func extractBinary(archive []byte, goos string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()
	want := binaryName(goos)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != want {
			continue
		}
		bin, err := io.ReadAll(io.LimitReader(tr, maxDownloadSize+1))
		if err != nil {
			return nil, fmt.Errorf("extract binary from archive: %w", err)
		}
		if len(bin) == 0 || len(bin) > maxDownloadSize {
			return nil, errors.New("binary in archive has invalid size")
		}
		return bin, nil
	}
	return nil, fmt.Errorf("archive does not contain %s", want)
}

// verifyBinary 校验二进制的格式 magic 与目标架构。release 未附带校验和，
// 这是替换前能做的最强完整性检查：损坏的下载、错配的架构都在这里拦下。
func verifyBinary(bin []byte, goos, goarch string) error {
	switch goos {
	case "linux":
		// ELF64 头：magic + EI_CLASS=2，e_machine 在偏移 18（小端）。
		if len(bin) < 20 || !bytes.Equal(bin[:4], []byte{0x7f, 'E', 'L', 'F'}) || bin[4] != 2 {
			return errors.New("not a 64-bit ELF binary")
		}
		var machine uint16
		switch goarch {
		case "amd64":
			machine = 62
		case "arm64":
			machine = 183
		default:
			return fmt.Errorf("unsupported architecture %s", goarch)
		}
		if binary.LittleEndian.Uint16(bin[18:20]) != machine {
			return errors.New("ELF architecture mismatch")
		}
	case "windows":
		// PE：MZ 头 + 0x3c 处的 PE 头偏移，machine 在 PE 签名后 2 字节。
		if len(bin) < 0x40 || bin[0] != 'M' || bin[1] != 'Z' {
			return errors.New("not a PE binary")
		}
		peOff := binary.LittleEndian.Uint32(bin[0x3c:0x40])
		if uint64(peOff)+6 > uint64(len(bin)) || !bytes.Equal(bin[peOff:peOff+4], []byte{'P', 'E', 0, 0}) {
			return errors.New("invalid PE header")
		}
		var machine uint16
		switch goarch {
		case "amd64":
			machine = 0x8664
		case "arm64":
			machine = 0xaa64
		default:
			return fmt.Errorf("unsupported architecture %s", goarch)
		}
		if binary.LittleEndian.Uint16(bin[peOff+4:peOff+6]) != machine {
			return errors.New("PE architecture mismatch")
		}
	case "darwin":
		// Mach-O 64（小端 magic 0xfeedfacf），cputype 在偏移 4。
		if len(bin) < 8 || binary.LittleEndian.Uint32(bin[:4]) != 0xfeedfacf {
			return errors.New("not a 64-bit Mach-O binary")
		}
		var cpu uint32
		switch goarch {
		case "amd64":
			cpu = 0x01000007
		case "arm64":
			cpu = 0x0100000c
		default:
			return fmt.Errorf("unsupported architecture %s", goarch)
		}
		if binary.LittleEndian.Uint32(bin[4:8]) != cpu {
			return errors.New("Mach-O architecture mismatch")
		}
	default:
		return fmt.Errorf("unsupported platform %s", goos)
	}
	return nil
}

// replaceBinary 用新二进制替换 exePath，保留原文件权限。
// 新文件先写到同目录临时文件（保证 rename 同文件系统、原子生效）：
//   - Unix：rename 直接覆盖，运行中的进程不受影响；
//   - Windows：运行中的 exe 无法覆盖也无法删除，但可以 rename ——
//     先把旧文件挪到 .old，再把新文件 rename 到位；第二步失败则把旧文件挪回来。
//     残留的 .old 在进程退出前删不掉，留待下次升级开头清理。
func replaceBinary(exePath string, bin []byte, goos string) error {
	info, err := os.Stat(exePath)
	if err != nil {
		return fmt.Errorf("stat current executable: %w", err)
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}

	tmp, err := os.CreateTemp(filepath.Dir(exePath), ".gitweb-upgrade-*")
	if err != nil {
		return fmt.Errorf("create temp file next to executable (need write permission on its directory): %w", err)
	}
	tmpName := tmp.Name()
	writeErr := func() error {
		defer tmp.Close()
		if _, err := tmp.Write(bin); err != nil {
			return err
		}
		return tmp.Chmod(mode)
	}()
	if writeErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write new binary: %w", writeErr)
	}

	if goos != "windows" {
		if err := os.Rename(tmpName, exePath); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("replace executable: %w", err)
		}
		return nil
	}

	old := exePath + ".old"
	os.Remove(old)
	if err := os.Rename(exePath, old); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("move current executable aside: %w", err)
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		os.Rename(old, exePath)
		os.Remove(tmpName)
		return fmt.Errorf("replace executable: %w", err)
	}
	os.Remove(old)
	return nil
}
