//go:build !windows

package main

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	rawMu        sync.Mutex
	savedTermios syscall.Termios
	termiosValid bool
)

// initConsoleColor Unix 终端原生支持 ANSI 颜色，无需额外设置。
func initConsoleColor() bool { return true }

// setRawMode 进入/退出原始输入模式（关闭回显、行缓冲与信号生成）。
func setRawMode(raw bool) bool {
	rawMu.Lock()
	defer rawMu.Unlock()
	fd := os.Stdin.Fd()
	var t syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)), 0, 0, 0); errno != 0 {
		return false
	}
	if raw {
		if !termiosValid {
			savedTermios = t
			termiosValid = true
		}
		t.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
		t.Iflag &^= syscall.IXON | syscall.ICRNL
		t.Cc[syscall.VMIN] = 1
		t.Cc[syscall.VTIME] = 0
	} else {
		if !termiosValid {
			return false
		}
		t = savedTermios
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t)), 0, 0, 0); errno != 0 {
		return false
	}
	return true
}
