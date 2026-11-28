package upgrade

import (
	"strconv"
	"strings"
)

// version 是解析后的语义化版本核心部分。
type version struct {
	nums [3]int // major.minor.patch，缺省段补 0
	pre  string // 预发布后缀（不含前导 '-'）；空 = 正式版
}

// CompareVersions 比较两个形如 v1.2.3 / v1.2.3-rc1 的版本号，返回 -1、0、1。
//
// ok 为 false 表示至少一方解析不了（例如开发版 "dev"）：调用方应放弃
// 「这是升级还是降级」的判断，只做普通确认，而不是猜一个方向。
// 规则按语义化版本的核心部分：主/次/修订逐段数值比较；同号时预发布版小于正式版。
func CompareVersions(a, b string) (int, bool) {
	va, ok := parseVersion(a)
	if !ok {
		return 0, false
	}
	vb, ok := parseVersion(b)
	if !ok {
		return 0, false
	}
	for i := range va.nums {
		switch {
		case va.nums[i] < vb.nums[i]:
			return -1, true
		case va.nums[i] > vb.nums[i]:
			return 1, true
		}
	}
	switch {
	case va.pre == vb.pre:
		return 0, true
	case va.pre == "":
		return 1, true // 正式版 > 同号预发布
	case vb.pre == "":
		return -1, true
	}
	return comparePrerelease(va.pre, vb.pre), true
}

// comparePrerelease 按 ASCII 点分段比较：纯数字段按数值比，数字段小于字母段，
// 其余按字典序。足够区分 rc1/rc2/beta 这类常见后缀。
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aNum := strconv.Atoi(as[i])
		bn, bNum := strconv.Atoi(bs[i])
		switch {
		case aNum == nil && bNum == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aNum == nil:
			return -1 // 数字段 < 字母段
		case bNum == nil:
			return 1
		default:
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// parseVersion 解析 vX[.Y[.Z]][-pre][+build]。要求至少 major.minor 两段。
func parseVersion(s string) (version, bool) {
	var out version
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return out, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		out.pre = s[i+1:]
		s = s[:i]
		if out.pre == "" {
			return out, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out.nums[i] = n
	}
	return out, true
}
