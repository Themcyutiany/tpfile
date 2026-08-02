//go:build !windows

package main

import (
	"os"
	"sync"
	"sync/atomic"
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

// startInputReader 启动整个会话唯一一个终端输入读取协程。
// 仅在 armed 为 true（行编辑器激活）时把按键字节送入通道；其余时间读取并
// 丢弃，避免输入在行与行之间堆积，也避免多个阻塞读取者互相争抢输入
// （表现为打不进字、Ctrl+C 失效）。
func startInputReader(ch chan<- inputEvent, armed *atomic.Bool) {
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 && armed.Load() {
				ch <- inputEvent{b: buf[0]}
			}
			if err != nil {
				ch <- inputEvent{eof: true}
				return
			}
		}
	}()
}
