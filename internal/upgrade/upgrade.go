// Package upgrade 实现自更新：从源仓库的 GitHub release 下载最新二进制并替换当前可执行文件。
//
// 发布产物的唯一来源是 GitHub Releases（发布流水线跑在 GitHub Actions 上）：
// release API 为 https://api.github.com/repos/<owner>/<repo>/releases/latest，
// asset 命名与 .github/workflows/release.yml 一致：gitweb-<os>-<arch>.tar.gz，
// 包内仅一个 gitweb。目标平台只有 linux/darwin（amd64/arm64）。
//
// 下载链路：统一走 API asset 端点（Accept: application/octet-stream），公开与
// 私有仓库行为一致，token 在该端点有效；browser_download_url 对私有仓库带不上
// 凭据，只在 API url 缺失时兜底。
//
// 安全边界：
//   - 仅 HTTPS。API 请求严格同源；asset 下载必然被 GitHub 重定向到其资产 CDN，
//     因此重定向目标只放行同源或固定的 GitHub CDN 域名白名单（端口限 443），
//     任何降级或白名单外的主机直接拒绝，跳数显式封顶。
//   - token 只发往 API 源站；跨源重定向跳显式剥离 Authorization。
//   - 对外错误一律固定分类文案，不携带 URL、tag、路径或底层网络/文件系统错误
//     —— 下载路径含 release tag，安装目录可能含版本号。
//   - 归档完整读完（gzip 尾部 CRC/长度校验必须执行），拒绝重复目标条目，
//     解压总量有预算；二进制做结构级校验；API 若提供 digest 字段则强制比对，
//     否则 release 附带 <asset>.sha256 校验和 asset 时强制比对。
//
// 按需求约定，本包任何错误信息与日志都不得包含版本号/tag 名。
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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultBaseURL 是 GitHub API 根地址。仅 HTTPS。
	DefaultBaseURL = "https://api.github.com"
	// DefaultRepo 是发布产物的源仓库（owner/name），可用 GITWEB_UPGRADE_REPO 覆盖。
	DefaultRepo = "fimreal/gitweb"

	// maxDownloadSize 限制单个 asset 的下载体积，防止异常响应耗尽内存。
	maxDownloadSize = 256 << 20
	// maxChecksumSize 校验和文件只有几十字节。
	maxChecksumSize = 4 << 10
	// maxRedirects 显式限制重定向跳数：自定义 CheckRedirect 会覆盖 net/http
	// 的默认上限，不自己实现的话同源循环会一直转到客户端超时。
	maxRedirects = 10
	// minBinarySize 真实可执行文件远大于此；挡住只有头部字段的伪造/截断产物。
	minBinarySize = 4096
)

// assetCDNHosts 是 GitHub 资产下载重定向的固定目标域名。GitHub 的 asset
// 下载（含 API asset 端点）会 302 到这些 CDN 主机上的签名地址，严格同源
// 会挡死合法链路，因此对重定向单独放行这份白名单；端口限 443。
var assetCDNHosts = map[string]bool{
	"github.com":                            true,
	"objects.githubusercontent.com":         true,
	"release-assets.githubusercontent.com":  true,
	"github-releases.githubusercontent.com": true,
}

// errCrossOrigin 是重定向/asset 地址越出允许链路时的固定错误，不含目标 URL。
var errCrossOrigin = errors.New("blocked redirect or asset URL outside the HTTPS update origins")

// errTooManyRedirects 是跳数封顶的固定错误。两者都是固定文案，
// 在 doGET 里按 sentinel 透传，不被归类成笼统的 network error。
var errTooManyRedirects = errors.New("too many redirects")

// Options 控制一次自更新。零值字段取默认；GOOS/GOARCH/ExecutablePath 供测试注入。
type Options struct {
	BaseURL        string // GitHub API 根地址，必须 HTTPS，默认 DefaultBaseURL
	Repo           string // owner/name，默认 DefaultRepo
	Token          string // GitHub access token；私有仓库必填，公开仓库可留空
	GOOS           string // 目标平台（linux/darwin），默认 runtime.GOOS
	GOARCH         string // 目标架构（amd64/arm64），默认 runtime.GOARCH
	ExecutablePath string // 要替换的可执行文件路径，默认当前进程
	Client         *http.Client
}

// Upgrade 下载最新 release 的二进制并替换当前可执行文件。
// 下载、解包、校验的任何一步失败都直接返回错误，原文件保持不动。
func Upgrade(ctx context.Context, opts Options) error {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return errors.New("update source must be an https URL")
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
	opts.Client.CheckRedirect = redirectPolicy(base)

	assetURL, checksumURL, digest, err := latestAssetURL(ctx, opts, base)
	if err != nil {
		return err
	}
	archive, err := download(ctx, opts, base, assetURL, maxDownloadSize, "release asset")
	if err != nil {
		return err
	}
	// digest 来自 release API 本身，优先于同源下载的 .sha256 sidecar。
	if digest != "" {
		if err := verifyDigest(digest, archive); err != nil {
			return err
		}
	} else if checksumURL != "" {
		if err := verifyChecksum(ctx, opts, base, checksumURL, archive); err != nil {
			return err
		}
	}
	bin, err := extractBinary(archive)
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
			return errors.New("locate current executable failed")
		}
		exePath, err = filepath.EvalSymlinks(exePath)
		if err != nil {
			return errors.New("resolve executable path failed")
		}
	}
	return replaceBinary(exePath, bin)
}

// redirectPolicy 每一跳重定向都重新校验：跳数封顶、目标必须在允许链路内，
// 跨源跳显式剥离 Authorization —— token 只属于 API 源站。
func redirectPolicy(base *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errTooManyRedirects
		}
		if !allowedAssetURL(base, req.URL) {
			return errCrossOrigin
		}
		if !sameOrigin(base, req.URL) {
			req.Header.Del("Authorization")
		}
		return nil
	}
}

// sameOrigin 要求 scheme、主机名、有效端口与源站全等 —— 子域、端口变化、
// HTTPS 降级都算跨源。按主机名后缀判断会放行子域，这里刻意全等。
func sameOrigin(base, target *url.URL) bool {
	return target != nil && target.Scheme == "https" &&
		target.Hostname() == base.Hostname() && effectivePort(target) == effectivePort(base)
}

// allowedAssetURL 是 asset 下载链路的目标白名单：同源，或 GitHub 资产 CDN
// 的固定域名（默认端口）。白名单外的主机与任何非 HTTPS 目标一律拒绝。
func allowedAssetURL(base, target *url.URL) bool {
	if target == nil || target.Scheme != "https" {
		return false
	}
	if sameOrigin(base, target) {
		return true
	}
	return assetCDNHosts[strings.ToLower(target.Hostname())] && effectivePort(target) == "443"
}

// effectivePort 归一化缺省端口：链路强制 HTTPS，缺省即 443。
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

// assetName 与 release.yml 的打包命名一一对应。
func assetName(goos, goarch string) string {
	return fmt.Sprintf("gitweb-%s-%s.tar.gz", goos, goarch)
}

// doGET 发起 GET；token 只附加在与 API 源站严格同源的目标上。
// 网络错误不透出底层 url.Error —— 它携带完整 URL（含 release tag）。
func doGET(ctx context.Context, opts Options, base *url.URL, rawURL, label, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s request failed", label)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if u, err := url.Parse(rawURL); err == nil && opts.Token != "" && sameOrigin(base, u) {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		// 重定向策略的拒绝是固定文案，按 sentinel 透传保留分类；
		// 其余网络错误不透出底层 url.Error —— 它携带完整 URL（含 release tag）。
		if errors.Is(err, errCrossOrigin) {
			return nil, errCrossOrigin
		}
		if errors.Is(err, errTooManyRedirects) {
			return nil, errTooManyRedirects
		}
		return nil, fmt.Errorf("%s request failed (network error)", label)
	}
	return resp, nil
}

// latestAssetURL 查询最新 release，选出当前平台 asset 的 API 下载地址、
// 可选校验和 sidecar 与可选 digest。刻意不解析、不返回 tag 名 ——
// 版本号不允许出现在任何输出里。
func latestAssetURL(ctx context.Context, opts Options, base *url.URL) (assetURL, checksumURL, digest string, err error) {
	api := fmt.Sprintf("%s/repos/%s/releases/latest", opts.BaseURL, opts.Repo)
	resp, err := doGET(ctx, opts, base, api, "release API", "application/vnd.github+json")
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", "", errors.New("no published release found on the update server")
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("release API returned HTTP %d", resp.StatusCode)
	}
	var rel struct {
		Assets []struct {
			Name               string `json:"name"`
			URL                string `json:"url"`
			BrowserDownloadURL string `json:"browser_download_url"`
			Digest             string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", "", "", errors.New("parse release response failed")
	}
	want := assetName(opts.GOOS, opts.GOARCH)
	hasChecksum := false
	for _, a := range rel.Assets {
		// 优先 API asset 端点：私有仓库的 browser_download_url 带不上凭据。
		dl := a.URL
		if dl == "" {
			dl = a.BrowserDownloadURL
		}
		switch a.Name {
		case want:
			assetURL, digest = dl, a.Digest
		case want + ".sha256":
			hasChecksum, checksumURL = true, dl
		}
	}
	if assetURL == "" {
		return "", "", "", fmt.Errorf("latest release has no asset for %s/%s", opts.GOOS, opts.GOARCH)
	}
	// 声明了校验和 asset 却没有下载地址是发布异常，不能静默降级成「无校验和」。
	if hasChecksum && checksumURL == "" {
		return "", "", "", errors.New("checksum asset is present but has no download URL")
	}
	// API 返回的地址同样要过链路白名单，恶意/被篡改的响应不能把请求引向别处。
	if u, err := url.Parse(assetURL); err != nil || !allowedAssetURL(base, u) {
		return "", "", "", errCrossOrigin
	}
	if checksumURL != "" {
		if u, err := url.Parse(checksumURL); err != nil || !allowedAssetURL(base, u) {
			return "", "", "", errCrossOrigin
		}
	}
	return assetURL, checksumURL, digest, nil
}

func download(ctx context.Context, opts Options, base *url.URL, rawURL string, limit int64, label string) ([]byte, error) {
	if u, err := url.Parse(rawURL); err != nil || !allowedAssetURL(base, u) {
		return nil, errCrossOrigin
	}
	resp, err := doGET(ctx, opts, base, rawURL, label, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s download returned HTTP %d", label, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s failed", label)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds size limit", label)
	}
	return data, nil
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// verifyDigest 比对 GitHub release API 返回的 digest 字段（sha256:<64hex>）。
// 字段存在就必须通过：格式非法、摘要不符都拒绝替换。
func verifyDigest(digest string, archive []byte) error {
	hexSum, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !sha256Hex.MatchString(hexSum) {
		return errors.New("release asset digest is malformed")
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != hexSum {
		return errors.New("release asset digest mismatch")
	}
	return nil
}

// verifyChecksum 比对 release 附带的 <asset>.sha256（sha256sum 格式，取首个字段）。
// 校验和存在就必须通过：下载失败、格式非法、摘要不符都拒绝替换。
func verifyChecksum(ctx context.Context, opts Options, base *url.URL, checksumURL string, archive []byte) error {
	data, err := download(ctx, opts, base, checksumURL, maxChecksumSize, "checksum")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || !sha256Hex.MatchString(fields[0]) {
		return errors.New("checksum file is malformed")
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != fields[0] {
		return errors.New("release asset checksum mismatch")
	}
	return nil
}

// budgetReader 给解压流加总量预算：tar 解析与末尾排空共用同一份额度，
// 高压缩比的垃圾条目（gzip 炸弹）不能无限消耗 CPU/时间。
type budgetReader struct {
	r         io.Reader
	remaining int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errors.New("archive exceeds decompressed size limit")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// extractBinary 从 tar.gz 中解出唯一的二进制条目 gitweb。只按条目基名匹配、
// 内容写入调用方指定的路径，归档里的路径字段不参与落盘，天然免疫路径穿越。
// 找到目标后继续读完整个归档：gzip 尾部的 CRC/长度校验只有在流被完整消费时
// 才会执行，截断或尾部损坏的包必须在这里被拒绝；重复目标条目同样拒绝。
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()
	var bin []byte
	stream := &budgetReader{r: gz, remaining: maxDownloadSize}
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "gitweb" {
			continue
		}
		if bin != nil {
			return nil, errors.New("archive contains duplicate binary entries")
		}
		bin, err = io.ReadAll(io.LimitReader(tr, maxDownloadSize+1))
		if err != nil {
			return nil, fmt.Errorf("extract binary from archive: %w", err)
		}
		if len(bin) == 0 || len(bin) > maxDownloadSize {
			return nil, errors.New("binary in archive has invalid size")
		}
	}
	// 排空剩余字节，触发 gzip 尾部 CRC32/ISIZE 校验。
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return nil, fmt.Errorf("archive integrity check failed: %w", err)
	}
	if bin == nil {
		return nil, errors.New("archive does not contain gitweb")
	}
	return bin, nil
}

// verifyBinary 对二进制做结构级校验：格式 magic、目标架构、头部表完整性与
// 文件内偏移边界。release 未提供 digest/校验和时这是替换前能做的最强检查；
// 它验证的是格式完整性而非来源真实性 —— 来源由 HTTPS 链路保证，
// digest 或 <asset>.sha256 存在时另有摘要比对。
func verifyBinary(bin []byte, goos, goarch string) error {
	if len(bin) < minBinarySize {
		return errors.New("binary is too small to be a real executable")
	}
	switch goos {
	case "linux":
		return verifyELF(bin, goarch)
	case "darwin":
		return verifyMachO(bin, goarch)
	default:
		return fmt.Errorf("unsupported platform %s", goos)
	}
}

func verifyELF(bin []byte, goarch string) error {
	// ELF64 头：magic、EI_CLASS=2、小端；e_phoff/e_phentsize/e_phnum 在 32/54/56。
	if !bytes.Equal(bin[:4], []byte{0x7f, 'E', 'L', 'F'}) || bin[4] != 2 || bin[5] != 1 {
		return errors.New("not a little-endian 64-bit ELF binary")
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
	phoff := binary.LittleEndian.Uint64(bin[32:40])
	phentsize := binary.LittleEndian.Uint16(bin[54:56])
	phnum := binary.LittleEndian.Uint16(bin[56:58])
	// 边界一律用减法形式：phoff + phnum*phentsize 这类无符号加法可被
	// 畸形头部溢出绕回，随后切片越界 panic。
	size := uint64(len(bin))
	if phnum == 0 || phentsize < 56 || phoff == 0 || phoff > size ||
		uint64(phnum)*uint64(phentsize) > size-phoff {
		return errors.New("ELF program header table is invalid")
	}
	loads := 0
	for i := 0; i < int(phnum); i++ {
		ph := bin[phoff+uint64(i)*uint64(phentsize):]
		pType := binary.LittleEndian.Uint32(ph[0:4])
		pOffset := binary.LittleEndian.Uint64(ph[8:16])
		pFilesz := binary.LittleEndian.Uint64(ph[32:40])
		if pOffset > size || pFilesz > size-pOffset {
			return errors.New("ELF segment extends past end of file")
		}
		if pType == 1 { // PT_LOAD
			loads++
		}
	}
	if loads == 0 {
		return errors.New("ELF has no loadable segment")
	}
	return nil
}

func verifyMachO(bin []byte, goarch string) error {
	// Mach-O 64（小端 magic 0xfeedfacf）：cputype 在偏移 4，
	// ncmds/sizeofcmds 在 16/20，load command 链从 32 开始。
	if binary.LittleEndian.Uint32(bin[:4]) != 0xfeedfacf {
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
	ncmds := binary.LittleEndian.Uint32(bin[16:20])
	sizeofcmds := binary.LittleEndian.Uint32(bin[20:24])
	if ncmds == 0 || uint64(32)+uint64(sizeofcmds) > uint64(len(bin)) {
		return errors.New("Mach-O load commands are invalid")
	}
	off := uint64(32)
	segments := 0
	for i := 0; i < int(ncmds); i++ {
		if off+8 > uint64(len(bin)) {
			return errors.New("Mach-O load command extends past end of file")
		}
		cmd := binary.LittleEndian.Uint32(bin[off : off+4])
		cmdsize := binary.LittleEndian.Uint32(bin[off+4 : off+8])
		if cmdsize < 8 || off+uint64(cmdsize) > uint64(len(bin)) {
			return errors.New("Mach-O load command extends past end of file")
		}
		if cmd == 0x19 { // LC_SEGMENT_64
			segments++
		}
		off += uint64(cmdsize)
	}
	if segments == 0 {
		return errors.New("Mach-O has no segment load command")
	}
	return nil
}

// renameFn 可在测试中注入失败，验证替换失败时原文件不动。
var renameFn = os.Rename

// replaceBinary 用新二进制替换 exePath，保留原文件权限：新文件先写到同目录
// 临时文件（保证 rename 同文件系统），再 rename 原子覆盖 —— 运行中的进程
// 不受影响，任何一步失败原文件都保持不动。
//
// 所有错误都是固定分类文案，不含路径与底层错误：安装目录可能含版本号，
// PathError 会把 tag 写进日志。
func replaceBinary(exePath string, bin []byte) error {
	info, err := os.Stat(exePath)
	if err != nil {
		return errors.New("stat current executable failed")
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}

	tmp, err := os.CreateTemp(filepath.Dir(exePath), ".gitweb-upgrade-*")
	if err != nil {
		return errors.New("create temp file next to executable failed (need write permission on its directory)")
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
		return errors.New("write new binary failed")
	}
	if err := renameFn(tmpName, exePath); err != nil {
		os.Remove(tmpName)
		return errors.New("replace executable failed")
	}
	return nil
}
