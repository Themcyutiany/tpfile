//go:build windows

package main

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	k32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetMode          = k32.NewProc("GetConsoleMode")
	procSetMode          = k32.NewProc("SetConsoleMode")
	procReadConsoleInput = k32.NewProc("ReadConsoleInputW")
	rawMu                sync.Mutex
	savedMode            uint32
	modeValid            bool
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

// ---- Windows 控制台输入记录（见 wincon.h）----

// keyEventRecord 对应 KEY_EVENT_RECORD。
type keyEventRecord struct {
	bKeyDown          uint32
	wRepeatCount      uint16
	wVirtualKeyCode   uint16
	wVirtualScanCode  uint16
	uChar             uint16
	dwControlKeyState uint32
}

// inputRecord 对应 INPUT_RECORD（EventType 之后有 2 字节对齐填充）。
type inputRecord struct {
	eventType uint16
	_         uint16
	keyEvent  keyEventRecord
}

const keyEventType = 0x0001 // INPUT_RECORD 的 KEY_EVENT

// ---- 常用虚拟键码与修饰键 ----

const (
	vkBack   = 0x08
	vkTab    = 0x09
	vkReturn = 0x0D
	vkEscape = 0x1B
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkDelete = 0x2E

	leftCtrl  = 0x0008
	rightCtrl = 0x0004
)

// startInputReader 启动整个会话唯一一个 Windows 控制台输入读取协程。
// 使用 ReadConsoleInputW 读取按键记录：按下即返回（避免逐字节 ReadFile 的
// 输入延迟与抬起事件延迟），自行把特殊键翻译成与终端一致的字节序列。
// 仅在 armed 为 true（行编辑器激活）时把字节送入编辑通道。
func startInputReader(ch chan<- inputEvent, armed *atomic.Bool) {
	go func() {
		h := os.Stdin.Fd()
		var rec inputRecord
		var highSurrogate uint16
		for {
			var n uint32
			r1, _, _ := procReadConsoleInput.Call(h, uintptr(unsafe.Pointer(&rec)), 1, uintptr(unsafe.Pointer(&n)))
			if r1 == 0 || n == 0 {
				ch <- inputEvent{eof: true}
				return
			}
			if rec.eventType != keyEventType {
				continue
			}
			k := &rec.keyEvent
			if k.bKeyDown == 0 {
				continue // 忽略按键抬起，避免输入延迟
			}
			if !armed.Load() {
				continue // 非编辑状态直接丢弃
			}
			repeat := int(k.wRepeatCount)
			if repeat < 1 {
				repeat = 1
			}
			if repeat > 64 {
				repeat = 64
			}
			if k.uChar != 0 {
				bs := keyCharBytes(k.uChar, &highSurrogate)
				for i := 0; i < repeat; i++ {
					for _, b := range bs {
						ch <- inputEvent{b: b}
					}
				}
				continue
			}
			if seq := keySeqFromVK(k.wVirtualKeyCode, k.dwControlKeyState); len(seq) > 0 {
				for i := 0; i < repeat; i++ {
					for _, b := range seq {
						ch <- inputEvent{b: b}
					}
				}
			}
		}
	}()
}

// keyCharBytes 把单个 UTF-16 码元转成 UTF-8 字节（处理代理对）。
func keyCharBytes(u uint16, highSurrogate *uint16) []byte {
	if *highSurrogate != 0 {
		if u >= 0xDC00 && u <= 0xDFFF {
			r := utf16.DecodeRune(rune(*highSurrogate), rune(u))
			*highSurrogate = 0
			return []byte(string(r))
		}
		*highSurrogate = 0
		return []byte("\uFFFD")
	}
	if u >= 0xD800 && u <= 0xDBFF {
		*highSurrogate = u
		return nil
	}
	return []byte(string(rune(u)))
}

// keySeqFromVK 把特殊键（无字符）翻译成字节序列，与终端转义序列保持一致。
func keySeqFromVK(vk uint16, state uint32) []byte {
	switch vk {
	case vkLeft:
		return []byte("\x1b[D")
	case vkRight:
		return []byte("\x1b[C")
	case vkUp:
		return []byte("\x1b[A")
	case vkDown:
		return []byte("\x1b[B")
	case vkHome:
		return []byte("\x1b[H")
	case vkEnd:
		return []byte("\x1b[F")
	case vkDelete:
		return []byte("\x1b[3~")
	case vkBack:
		return []byte{0x7f}
	case vkTab:
		return []byte{0x09}
	case vkReturn:
		return []byte{0x0d}
	case vkEscape:
		return []byte{0x1b}
	}
	if state&(leftCtrl|rightCtrl) != 0 {
		switch vk {
		case 'C':
			return []byte{0x03} // Ctrl+C
		case 'D':
			return []byte{0x04} // Ctrl+D
		case 'Z':
			return []byte{0x1a} // Ctrl+Z
		}
	}
	return nil
}
