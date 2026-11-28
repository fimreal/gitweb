//go:build darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal 用 TIOCGETA 判断 fd 是否真的是终端（darwin 的 termios 读取 ioctl）。
// 同 linux 侧：/dev/null 也是字符设备，不能只靠 ModeCharDevice 判断。
func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGETA),
		uintptr(unsafe.Pointer(&termios)), 0, 0, 0,
	)
	return errno == 0
}
