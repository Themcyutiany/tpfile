package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// readLines 启动一个 goroutine 读取用户的命令行输入（终端下带 Tab 补全、
// 左右移动等编辑能力；非终端自动退化为逐行扫描）。
func readLines(ctx context.Context) <-chan string {
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		lr := newLineReader()
		for {
			line, ok := lr.next()
			if !ok {
				return
			}
			select {
			case ch <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// lineReader 统一终端交互读取与非终端逐行读取。
type lineReader struct {
	inter   bool
	scanner *bufio.Scanner
}

func newLineReader() *lineReader {
	lr := &lineReader{}
	// 终端交互模式要求：stdin/stdout 都是终端、颜色已启用、能进入原始模式。
	lr.inter = isTerminal(os.Stdin) && isTerminal(os.Stdout) && colorOn && setRawMode(true)
	if lr.inter {
		setRawMode(false) // 每次 readLineInteractive 再进入原始模式
	} else {
		lr.scanner = bufio.NewScanner(os.Stdin)
	}
	return lr
}

func (lr *lineReader) next() (string, bool) {
	if !lr.inter {
		if !lr.scanner.Scan() {
			return "", false
		}
		return strings.TrimSpace(lr.scanner.Text()), true
	}
	return readLineInteractive()
}

// readLineInteractive 在原始模式下读取一行：支持左右/Home/End 移动光标、
// 退格、删除、Ctrl+D、Tab 补全本地文件路径、Ctrl+C(输出 stop)。
func readLineInteractive() (string, bool) {
	if !setRawMode(true) {
		return readLineScanner()
	}
	defer setRawMode(false)

	outMu.Lock()
	editActive = true
	editBuf = editBuf[:0]
	editPos = 0
	promptDirty = false
	outMu.Unlock()

	byteCh := make(chan byte, 64)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				select {
				case byteCh <- buf[0]:
				default:
				}
			}
			if err != nil {
				close(byteCh)
				return
			}
		}
	}()

	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()

	var (
		escBuf   []byte
		escStart time.Time
		pending  []byte // UTF-8 多字节输入缓冲
	)

	for {
		select {
		case b, ok := <-byteCh:
			if !ok {
				outMu.Lock()
				editActive = false
				outMu.Unlock()
				return "", false
			}
			if len(escBuf) > 0 {
				escBuf = append(escBuf, b)
				if done, esc := consumeEscSeq(escBuf); done {
					handleEscSeq(esc)
					escBuf = nil
				} else if !isEscPrefix(escBuf) {
					escBuf = nil // 不是合法转义序列，丢弃
				}
				continue
			}
			switch {
			case b == 0x1b:
				escBuf = append(escBuf, b)
				escStart = time.Now()
			case b >= 0x80:
				pending = append(pending, b)
				if utf8.FullRune(pending) {
					r, _ := utf8.DecodeRune(pending)
					pending = pending[:0]
					insertEditRune(r)
				}
			default:
				if done, line := handleCtrlKey(b); done {
					outMu.Lock()
					editActive = false
					promptOnScreen = false
					outMu.Unlock()
					return line, true
				}
			}
		case <-tick.C:
			outMu.Lock()
			if promptDirty && promptShown {
				drawInputLocked()
				promptDirty = false
			}
			outMu.Unlock()
			if len(escBuf) > 0 && time.Since(escStart) > 100*time.Millisecond {
				escBuf = nil // 单独按 ESC 无后续，丢弃
			}
		}
	}
}

// readLineScanner 退化的逐行读取（非终端 stdin）。
func readLineScanner() (string, bool) {
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", false
	}
	return strings.TrimSpace(sc.Text()), true
}

// handleCtrlKey 处理控制字节；返回 done=true 表示一行输入结束。
func handleCtrlKey(b byte) (bool, string) {
	outMu.Lock()
	defer outMu.Unlock()
	switch b {
	case 0x03: // Ctrl+C
		fmt.Fprint(os.Stdout, "\r\n")
		promptOnScreen = false
		return true, "stop"
	case 0x04: // Ctrl+D：空行 EOF，否则删除光标处字符
		if len(editBuf) == 0 {
			fmt.Fprint(os.Stdout, "\r\n")
			promptOnScreen = false
			return true, ""
		}
		if editPos < len(editBuf) {
			editBuf = append(editBuf[:editPos], editBuf[editPos+1:]...)
			drawInputLocked()
		}
	case 0x0d, 0x0a: // Enter
		fmt.Fprint(os.Stdout, "\r\n")
		promptOnScreen = false
		return true, string(editBuf)
	case 0x7f, 0x08: // Backspace
		if editPos > 0 {
			editBuf = append(editBuf[:editPos-1], editBuf[editPos:]...)
			editPos--
			drawInputLocked()
		}
	case 0x09: // Tab：补全本地文件路径
		handleTabLocked()
	default:
		if b >= 0x20 {
			insertEditRuneLocked(rune(b))
		}
	}
	return false, ""
}

// insertEditRune 插入一个可打印字符（含 UTF-8 多字节）。
func insertEditRune(r rune) {
	outMu.Lock()
	defer outMu.Unlock()
	insertEditRuneLocked(r)
}

func insertEditRuneLocked(r rune) {
	if r < 0x20 && r != 0x09 {
		return
	}
	editBuf = append(editBuf, 0)
	copy(editBuf[editPos+1:], editBuf[editPos:])
	editBuf[editPos] = r
	editPos++
	drawInputLocked()
}

// handleTabLocked 执行 Tab 补全：唯一匹配直接补全；多匹配先补公共前缀，
// 无法再扩展时在下方列出候选。调用方需持有 outMu。
func handleTabLocked() {
	newBuf, newPos, cands := completePath(editBuf, editPos)
	if len(cands) == 0 {
		return
	}
	if len(cands) == 1 || string(newBuf) != string(editBuf) {
		editBuf, editPos = newBuf, newPos
		drawInputLocked()
		return
	}
	// 多个候选且公共前缀不再增长：候选覆盖当前输入行，再重绘输入行
	fmt.Fprint(os.Stdout, "\r\x1b[2K")
	for i, c := range cands {
		if i > 0 {
			fmt.Fprint(os.Stdout, "\n")
		}
		fmt.Fprint(os.Stdout, paint(dirColor, c))
	}
	fmt.Fprint(os.Stdout, "\n")
	drawInputLocked()
	promptDirty = false
}

// completePath 对输入中光标所在的当前词做本地文件路径补全。
// 返回补全后的缓冲、新光标位置，以及匹配到的候选（用于展示）。
func completePath(input []rune, pos int) ([]rune, int, []string) {
	start := pos
	for start > 0 && !unicode.IsSpace(input[start-1]) {
		start--
	}
	token := string(input[start:pos])

	var dirPart, prefix string
	if i := strings.LastIndexAny(token, `/\`); i >= 0 {
		dirPart = token[:i+1]
		prefix = token[i+1:]
	} else {
		prefix = token
	}
	base := "."
	if dirPart != "" {
		base = filepath.Clean(dirPart)
		if base == "." {
			base = "."
		}
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return input, pos, nil
	}
	var cands []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		cands = append(cands, name)
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return input, pos, nil
	}
	common := commonPrefix(cands)
	if len(common) > len(prefix) || len(cands) == 1 {
		full := dirPart + common
		if len(cands) == 1 {
			full = dirPart + cands[0]
		}
		newBuf := make([]rune, 0, len(input)-runeLen(token)+runeLen(full))
		newBuf = append(newBuf, input[:start]...)
		newBuf = append(newBuf, []rune(full)...)
		newBuf = append(newBuf, input[pos:]...)
		return newBuf, start + runeLen(full), cands
	}
	return input, pos, cands
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			if len(p) == 0 {
				return ""
			}
			p = p[:len(p)-1]
		}
	}
	return p
}

// consumeEscSeq 判断 escBuf 是否构成完整的终端转义序列。
func consumeEscSeq(buf []byte) (bool, []byte) {
	switch string(buf) {
	case "\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[3~":
		return true, append([]byte(nil), buf...)
	}
	return false, nil
}

func isEscPrefix(buf []byte) bool {
	s := string(buf)
	if s == "\x1b" || s == "\x1b[" {
		return true
	}
	if len(s) == 3 && s[0] == 0x1b && s[1] == '[' {
		// 等待第四个字节（如 [3~）
		return true
	}
	return false
}

// handleEscSeq 处理方向键等转义序列。
func handleEscSeq(seq []byte) {
	outMu.Lock()
	defer outMu.Unlock()
	switch string(seq) {
	case "\x1b[C": // 右
		if editPos < len(editBuf) {
			editPos++
			drawInputLocked()
		}
	case "\x1b[D": // 左
		if editPos > 0 {
			editPos--
			drawInputLocked()
		}
	case "\x1b[H": // Home
		if editPos != 0 {
			editPos = 0
			drawInputLocked()
		}
	case "\x1b[F": // End
		if editPos != len(editBuf) {
			editPos = len(editBuf)
			drawInputLocked()
		}
	case "\x1b[3~": // Delete
		if editPos < len(editBuf) {
			editBuf = append(editBuf[:editPos], editBuf[editPos+1:]...)
			drawInputLocked()
		}
	}
}
