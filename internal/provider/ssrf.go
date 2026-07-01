package provider

import (
	"net"
	"net/url"
	"strings"
)

// isAllowedHost 校验目标 host 是否允许访问，用于防止 SSRF（用户传入的 git_url
// 可能指向内网/环回）。默认拒绝私网、环回、链路本地、未分配地址；allow 列表
// 显式放行（支持 `*.gitea.*` 这类通配），deny 列表显式拒绝（优先级高于 allow）。
//
// host 可能为 `host:port` 形式，先拆出 host 再解析。
func isAllowedHost(host string, allow, deny []string) bool {
	h := hostOnly(host)

	// deny 优先
	for _, pat := range deny {
		if matchHostPattern(h, pat) {
			return false
		}
	}

	// 命中 allow 直接放行（含通配）
	for _, pat := range allow {
		if matchHostPattern(h, pat) {
			return true
		}
	}

	// 解析 IP，私网/环回/链路本地一律拒绝
	if ip := net.ParseIP(h); ip != nil {
		return !isPrivateIP(ip)
	}

	// 域名：解析后再逐个校验。解析失败（域名不存在）按拒绝处理，避免绕过。
	ips, err := net.LookupIP(h)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return false
		}
	}
	return true
}

// hostOnly 从 `host:port` 中取出 host（IPv6 的 `[::1]:port` 也处理）。
func hostOnly(h string) string {
	if strings.HasPrefix(h, "[") {
		if idx := strings.LastIndex(h, "]"); idx != -1 {
			return h[1:idx]
		}
	}
	if h, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return h
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

// matchHostPattern 支持通配：`*.gitea.*` 匹配 `gitea.example.com`、`a.gitea.b`。
func matchHostPattern(host, pat string) bool {
	host = strings.ToLower(host)
	pat = strings.ToLower(pat)
	if !strings.Contains(pat, "*") {
		return host == pat
	}
	// 把 pattern 按 * 切成段，host 必须依次包含各段，且首段需匹配开头、末段匹配结尾
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return host == parts[0]
	}
	if !strings.HasPrefix(host, parts[0]) {
		return false
	}
	if !strings.HasSuffix(host, parts[len(parts)-1]) {
		return false
	}
	rest := host
	for _, p := range parts {
		idx := strings.Index(rest, p)
		if idx == -1 {
			return false
		}
		rest = rest[idx+len(p):]
	}
	return true
}

// allowedURL 对一个完整 URL 做 SSRF 校验（解析 host 后调用 isAllowedHost）。
func allowedURL(rawURL string, allow, deny []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return isAllowedHost(u.Host, allow, deny)
}
