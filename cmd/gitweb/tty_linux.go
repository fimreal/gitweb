//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal 用 TCGETS 判断 fd 是否真的是终端。
// 不能用 File.Stat 的 ModeCharDevice：/dev/null 也是字符设备，会把
// 「stdin 重定向到 /dev/null」误判成交互式终端，从而跳过拒绝逻辑。
func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS),
		uintptr(unsafe.Pointer(&termios)), 0, 0, 0,
	)
	return errno == 0
}
