package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// 命令历史：支持在编辑器中用 ↑ / ↓ 浏览已提交的命令。
var (
	history   []string // 已提交的非空命令（最新在末尾）
	histIdx   int      // 当前浏览位置；-1 表示正在编辑新行
	histDraft []rune   // 从历史离开时保存的原输入草稿
)

// inputEvent 表示来自常驻输入读取协程的一个事件。
type inputEvent struct {
	b   byte // 一个输入字节
	eof bool // stdin 已关闭/出错
}

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
// 终端模式下整个会话只启动一个常驻读取协程（startInputReader），通过 armed
// 开关决定是否把按键送入编辑通道。这样全程只有一个读取者，不会出现多个
// 阻塞在 stdin 上的协程互相争抢输入（表现为打不进字、Ctrl+C 失效）。
type lineReader struct {
	inter   bool
	scanner *bufio.Scanner
	inputCh chan inputEvent
	armed   *atomic.Bool
}

func newLineReader() *lineReader {
	lr := &lineReader{}
	// 终端交互模式要求：stdin/stdout 都是终端、颜色已启用、能进入原始模式。
	lr.inter = isTerminal(os.Stdin) && isTerminal(os.Stdout) && colorOn && setRawMode(true)
	if lr.inter {
		setRawMode(false) // 每次 readLineInteractive 再进入原始模式
		lr.inputCh = make(chan inputEvent, 512)
		lr.armed = &atomic.Bool{}
		startInputReader(lr.inputCh, lr.armed)
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
	return readLineInteractive(lr.inputCh, lr.armed)
}

// readLineInteractive 在原始模式下读取一行：支持左右/Home/End 移动光标、
// 退格、删除、Ctrl+D、Tab 补全本地文件路径、Ctrl+C(输出 stop)。
func readLineInteractive(inputCh chan inputEvent, armed *atomic.Bool) (string, bool) {
	if !setRawMode(true) {
		return readLineScanner()
	}
	defer setRawMode(false)

	outMu.Lock()
	editActive = true
	editBuf = editBuf[:0]
	editPos = 0
	promptDirty = false
	histIdx = -1
	histDraft = nil
	outMu.Unlock()
	armed.Store(true)

	// 丢弃上一行遗留的按键字节，保证每行从干净状态开始
drainStale:
	for {
		select {
		case ev := <-inputCh:
			if ev.eof {
				armed.Store(false)
				outMu.Lock()
				editActive = false
				outMu.Unlock()
				return "", false
			}
		default:
			break drainStale
		}
	}

	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()

	var (
		escBuf   []byte
		escStart time.Time
		pending  []byte // UTF-8 多字节输入缓冲
	)

	finish := func(line string) (string, bool) {
		armed.Store(false)
		outMu.Lock()
		editActive = false
		promptOnScreen = false
		outMu.Unlock()
		return line, true
	}

	for {
		select {
		case ev, ok := <-inputCh:
			if !ok || ev.eof {
				armed.Store(false)
				outMu.Lock()
				editActive = false
				outMu.Unlock()
				return "", false
			}
			done, line, redraw := applyKey(ev.b, &escBuf, &escStart, &pending)
			// 同一批次内连续到达的按键合并成一次重绘，避免快速输入时逐字节刷屏
			for !done {
				select {
				case ev2, ok2 := <-inputCh:
					if !ok2 || ev2.eof {
						armed.Store(false)
						outMu.Lock()
						editActive = false
						outMu.Unlock()
						return "", false
					}
					var rd bool
					done, line, rd = applyKey(ev2.b, &escBuf, &escStart, &pending)
					redraw = redraw || rd
				default:
					goto drained
				}
			}
		drained:
			if done {
				return finish(line)
			}
			if redraw {
				outMu.Lock()
				drawInputLocked()
				outMu.Unlock()
			}
		case <-tick.C:
			outMu.Lock()
			if promptDirty && promptShown {
				drawInputLocked()
				promptDirty = false
			}
			outMu.Unlock()
			if len(escBuf) > 0 && time.Since(escStart) > 100*time.Millisecond {
				escBuf = nil
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

// applyKey 处理一个输入字节（或转义序列片段），更新编辑缓冲与光标。
// 返回 done=true 表示一行输入结束（line 为最终内容）；redraw=true 表示缓冲
// 发生变化、调用方需要重绘提示行。Tab 与转义序列内部自行重绘。
func applyKey(b byte, escBuf *[]byte, escStart *time.Time, pending *[]byte) (bool, string, bool) {
	if len(*escBuf) > 0 {
		*escBuf = append(*escBuf, b)
		if doneSeq, esc := consumeEscSeq(*escBuf); doneSeq {
			handleEscSeq(esc)
			*escBuf = nil
			return false, "", false
		}
		if !isEscPrefix(*escBuf) {
			*escBuf = nil // 不是合法转义序列，丢弃
		}
		return false, "", false
	}
	switch {
	case b == 0x1b:
		*escBuf = append(*escBuf, b)
		*escStart = time.Now()
		return false, "", false
	case b >= 0x80:
		*pending = append(*pending, b)
		if utf8.FullRune(*pending) {
			r, _ := utf8.DecodeRune(*pending)
			*pending = (*pending)[:0]
			outMu.Lock()
			insertEditRuneLocked(r)
			outMu.Unlock()
			return false, "", true
		}
		return false, "", false
	}

	outMu.Lock()
	defer outMu.Unlock()
	switch b {
	case 0x03: // Ctrl+C
		fmt.Fprint(os.Stdout, "\r\n")
		promptOnScreen = false
		return true, "stop", false
	case 0x04: // Ctrl+D：空行 EOF，否则删除光标处字符
		if len(editBuf) == 0 {
			fmt.Fprint(os.Stdout, "\r\n")
			promptOnScreen = false
			return true, "", false
		}
		if editPos < len(editBuf) {
			editBuf = append(editBuf[:editPos], editBuf[editPos+1:]...)
			return false, "", true
		}
	case 0x0d, 0x0a: // Enter
		fmt.Fprint(os.Stdout, "\r\n")
		promptOnScreen = false
		addHistory(string(editBuf))
		return true, string(editBuf), false
	case 0x7f, 0x08: // Backspace
		if editPos > 0 {
			editBuf = append(editBuf[:editPos-1], editBuf[editPos:]...)
			editPos--
			return false, "", true
		}
	case 0x09: // Tab：补全本地文件路径
		handleTabLocked()
		return false, "", false
	default:
		if b >= 0x20 {
			insertEditRuneLocked(rune(b))
			return false, "", true
		}
	}
	return false, "", false
}

// handleCtrlKey 处理单个控制字节（测试与兼容入口，不负责重绘）。
func handleCtrlKey(b byte) (bool, string) {
	var escBuf []byte
	var escStart time.Time
	var pending []byte
	done, line, _ := applyKey(b, &escBuf, &escStart, &pending)
	return done, line
}

func insertEditRuneLocked(r rune) {
	if r < 0x20 && r != 0x09 {
		return
	}
	editBuf = append(editBuf, 0)
	copy(editBuf[editPos+1:], editBuf[editPos:])
	editBuf[editPos] = r
	editPos++
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
	// 多个候选且公共前缀不再增长：先清掉进度行，候选覆盖当前输入行，再重绘输入行
	clearProgressLocked()
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
	case "\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[3~", "\x1b[Z",
		"\x1b[1~", "\x1b[2~", "\x1b[4~", "\x1b[5~", "\x1b[6~":
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
	case "\x1b[A": // 上：浏览历史中更早的命令
		histUp()
	case "\x1b[B": // 下：浏览历史中更新的命令 / 回到原输入
		histDown()
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
	case "\x1b[Z": // Shift+Tab：等同 Tab
		handleTabLocked()
	}
}

// addHistory 把提交的命令记入历史：去掉空行、去掉与上一条完全相同的重复，上限 500 条。
func addHistory(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(history) > 0 && history[len(history)-1] == line {
		return
	}
	history = append(history, line)
	if len(history) > 500 {
		history = history[1:]
	}
}

// histUp 向上浏览历史。首次按 ↑ 时保存当前输入草稿，之后逐条向前。
// 调用方需持有 outMu。
func histUp() {
	if len(history) == 0 {
		return
	}
	if histIdx < 0 {
		histDraft = append([]rune(nil), editBuf...)
		histIdx = len(history) - 1
	} else if histIdx > 0 {
		histIdx--
	}
	editBuf = []rune(history[histIdx])
	editPos = len(editBuf)
	drawInputLocked()
}

// histDown 向下浏览历史；到最新一条之后恢复原来的输入草稿。
// 调用方需持有 outMu。
func histDown() {
	if histIdx < 0 {
		return
	}
	if histIdx < len(history)-1 {
		histIdx++
		editBuf = []rune(history[histIdx])
	} else {
		histIdx = -1
		editBuf = append([]rune(nil), histDraft...)
	}
	editPos = len(editBuf)
	drawInputLocked()
}
