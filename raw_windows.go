//go:build windows

package main

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	k32         = syscall.NewLazyDLL("kernel32.dll")
	procGetMode = k32.NewProc("GetConsoleMode")
	procSetMode = k32.NewProc("SetConsoleMode")
	rawMu       sync.Mutex
	savedMode   uint32
	modeValid   bool
)

// initConsoleColor 为 Windows 控制台启用 VT 处理（颜色/清屏转义序列）。
func initConsoleColor() bool {
	h := os.Stdout.Fd()
	var mode uint32
	if r, _, _ := procGetMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	const enableVirtualTerminalProcessing = 0x0004
	if r, _, _ := procSetMode.Call(h, uintptr(mode|enableVirtualTerminalProcessing)); r == 0 {
		return false
	}
	return true
}

// setRawMode 进入/退出原始输入模式（关闭回显与行缓冲）。
func setRawMode(raw bool) bool {
	rawMu.Lock()
	defer rawMu.Unlock()
	h := os.Stdin.Fd()
	var mode uint32
	if r, _, _ := procGetMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	const (
		enableProcessedInput = 0x0001
		enableLineInput      = 0x0002
		enableEchoInput      = 0x0004
		enableVtInput        = 0x0200
	)
	if raw {
		if !modeValid {
			savedMode = mode
			modeValid = true
		}
		mode &^= enableProcessedInput | enableLineInput | enableEchoInput
		mode |= enableVtInput
	} else {
		if !modeValid {
			return false
		}
		mode = savedMode
	}
	if r, _, _ := procSetMode.Call(h, uintptr(mode)); r == 0 {
		return false
	}
	return true
}
