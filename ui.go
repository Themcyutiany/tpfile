package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// ---------- 终端状态 ----------

var (
	outMu          sync.Mutex
	progressLive   bool   // 当前行是否有一条未换行的进度条
	lastBarLine    string // 进度条当前显示的内容（重绘输入行时复用）
	promptShown    bool   // 是否处于等待输入的交互状态
	promptOnScreen bool   // 屏幕上是否已经画了 "> " 提示行
	promptDirty    bool   // 有消息输出后提示行需要重绘（由 prompt/编辑器定时刷新）
	editActive     bool   // 行编辑器是否正在工作（读取键盘输入中）
	editBuf        []rune // 编辑器当前输入缓冲
	editPos        int    // 编辑器光标位置（rune 下标）
)

// ---------- 颜色 ----------

// colorOn 仅在终端且支持 VT 时启用颜色。
var colorOn = false

func init() {
	colorOn = isTerminal(os.Stdout)
	if colorOn && os.Getenv("NO_COLOR") != "" {
		colorOn = false
	}
	if colorOn && !initConsoleColor() {
		colorOn = false
	}
}

const (
	promptColor = "1;36" // 提示符: 亮青色
	dirColor    = "34"   // 目录: 蓝色
	okColor     = "32"   // 成功: 绿色
	errColor    = "31"   // 错误: 红色
	infoColor   = "33"   // 用户事件: 黄色
	dimColor    = "90"   // 次要信息: 灰色
)

// paint 给文本加上 ANSI 颜色（非终端时原样返回）。
func paint(code, s string) string {
	if !colorOn {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// dispWidth 返回字符串在终端上占用的显示宽度：CJK 全角字符按 2 列计，
// 其余按 1 列（ASCII 控制符不计）。用于光标定位与列表对齐。
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r == 0:
			// 不计
		case r >= 0x1100 && (r <= 0x115F || r == 0x2329 || r == 0x232A ||
			(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) ||
			(r >= 0xAC00 && r <= 0xD7A3) || (r >= 0xF900 && r <= 0xFAFF) ||
			(r >= 0xFE30 && r <= 0xFE4F) || (r >= 0xFF00 && r <= 0xFF60) ||
			(r >= 0xFFE0 && r <= 0xFFE6) || (r >= 0x20000 && r <= 0x3FFFD)):
			w += 2
		default:
			w++
		}
	}
	return w
}

// ---------- 输出 ----------

// printLine 输出一行日志。等待输入时把消息放在提示行下方，连续多条消息不会
// 插入空行；消息输出后提示行由 prompt()/编辑器定时刷新重绘。
func printLine(format string, args ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	clearProgressLocked()
	if promptShown {
		if colorOn {
			// 终端模式：消息覆盖提示行，连续输出不插空行，提示行稍后统一重绘
			if promptOnScreen {
				fmt.Fprint(os.Stdout, "\r\x1b[2K")
			}
			fmt.Fprintf(os.Stdout, format+"\n", args...)
			promptOnScreen = false
			promptDirty = true
		} else {
			// 非终端（管道/重定向）：保持旧的逐行输出
			fmt.Fprint(os.Stdout, "\n")
			fmt.Fprintf(os.Stdout, format+"\n", args...)
			fmt.Fprint(os.Stdout, "> ")
		}
		return
	}
	fmt.Fprintf(os.Stdout, format+"\n", args...)
	promptOnScreen = false
}

// printOK / printErr / printInfo / printDim 是带颜色的 printLine 便捷封装。
func printOK(format string, args ...any)   { printLine(paint(okColor, fmt.Sprintf(format, args...))) }
func printErr(format string, args ...any)  { printLine(paint(errColor, fmt.Sprintf(format, args...))) }
func printInfo(format string, args ...any) { printLine(paint(infoColor, fmt.Sprintf(format, args...))) }
func printDim(format string, args ...any)  { printLine(paint(dimColor, fmt.Sprintf(format, args...))) }

// prompt 在开始等待输入时调用：需要时重绘提示行。
func prompt() {
	outMu.Lock()
	defer outMu.Unlock()
	if !colorOn {
		fmt.Fprintln(os.Stdout)
		fmt.Fprint(os.Stdout, "> ")
		promptShown = true
		return
	}
	if promptDirty || !promptOnScreen {
		drawInputLocked()
		promptDirty = false
	}
	promptShown = true
}

// clearPromptFlag 收到指令开始执行时调用，清掉提示行并复位状态。
func clearPromptFlag() {
	outMu.Lock()
	defer outMu.Unlock()
	clearProgressLocked()
	if colorOn && (promptShown || editActive) {
		fmt.Fprint(os.Stdout, "\r"+strings.Repeat(" ", 120)+"\r")
	}
	progressLive = false
	promptShown = false
	promptOnScreen = false
	promptDirty = false
	// 注意：不要在这里把 editActive 置为 false。editActive 只归行编辑器管理
	//（readLineInteractive 启动/结束时开关）；主循环可能在读取协程已经启动了
	// 下一行编辑器之后才执行到这里，一旦覆盖会导致“能输入但屏幕不显示”。
}
// drawInputLocked 重绘提示行（进度条存在时在上一行一并重绘），调用方需持有 outMu。
// 整行拼成一次写入，减少逐字节刷屏带来的输入卡顿。
func drawInputLocked() {
	var sb strings.Builder
	sb.WriteString("\r\x1b[2K") // 清当前行（提示行）
	if progressLive && lastBarLine != "" {
		sb.WriteString("\x1b[1A\r\x1b[2K") // 上一行是进度行，清掉
		sb.WriteString(lastBarLine)
		sb.WriteString("\n") // 提示符换到进度行下方一行
	}
	sb.WriteString(paint(promptColor, "> "))
	if editActive {
		sb.WriteString(string(editBuf))
		if editPos < runeLen(string(editBuf)) {
			fmt.Fprintf(&sb, "\x1b[%dD", dispWidth(string(editBuf[editPos:])))
		}
	}
	fmt.Fprint(os.Stdout, sb.String())
	promptOnScreen = true
}
func clearProgressLocked() {
	if !progressLive {
		return
	}
	// 光标位于进度行末尾（无提示符）或提示行末尾（有提示符）
	fmt.Fprint(os.Stdout, "\r\x1b[2K")
	if promptOnScreen {
		fmt.Fprint(os.Stdout, "\x1b[1A\r\x1b[2K")
	}
	progressLive = false
	lastBarLine = ""
	promptOnScreen = false
}

// clearProgressNow 清掉当前进度条（中断时使用，不留任何残留行）。
func clearProgressNow() {
	outMu.Lock()
	defer outMu.Unlock()
	clearProgressLocked()
	if promptOnScreen {
		fmt.Fprint(os.Stdout, "\r\x1b[2K")
		promptOnScreen = false
	}
}
// renderProgress 渲染一行进度：进度条独占一行，提示符在其下方一行（终端下用 \r 刷新）。
// 非终端只在 final 时输出一行。
func renderProgress(line string, final bool) {
	outMu.Lock()
	defer outMu.Unlock()
	if !isTerminal(os.Stdout) {
		if final {
			fmt.Fprintln(os.Stdout, line)
		}
		return
	}
	clearProgressLocked()
	if promptOnScreen {
		fmt.Fprint(os.Stdout, "\r\x1b[2K")
	}
	fmt.Fprint(os.Stdout, line)
	lastBarLine = line
	progressLive = true
	promptOnScreen = false
	if final {
		fmt.Fprintln(os.Stdout)
		progressLive = false
		lastBarLine = ""
		if promptShown {
			promptDirty = true
		}
		return
	}
	if promptShown || editActive {
		// 进度条独占一行，提示符换到下一行，传输期间照常可以输入
		fmt.Fprintln(os.Stdout)
		fmt.Fprint(os.Stdout, paint(promptColor, "> "))
		if editActive {
			fmt.Fprint(os.Stdout, string(editBuf))
			if editPos < runeLen(string(editBuf)) {
				fmt.Fprintf(os.Stdout, "\x1b[%dD", dispWidth(string(editBuf[editPos:])))
			}
		}
		promptOnScreen = true
	}
}

// ---------- 目录列表（Linux ls 风格） ----------

// printList 以 Linux ls 风格输出目录列表：目录蓝色并带 / 后缀，多列对齐。
func printList(header string, entries []string) {
	if header != "" {
		printDim("%s", header)
	}
	if len(entries) == 0 {
		printDim("(空目录)")
		return
	}
	width := termWidth()
	maxW := 0
	for _, e := range entries {
		if w := dispWidth(e); w > maxW {
			maxW = w
		}
	}
	colW := maxW + 2
	cols := width / colW
	if cols < 1 {
		cols = 1
	}
	rows := (len(entries) + cols - 1) / cols
	for r := 0; r < rows; r++ {
		var sb strings.Builder
		for c := 0; c < cols; c++ {
			i := c*rows + r
			if i >= len(entries) {
				continue
			}
			name := entries[i]
			cell := padRight(name, colW)
			if strings.HasSuffix(name, "/") {
				cell = paint(dirColor, cell)
			}
			sb.WriteString(cell)
		}
		printLine("%s", strings.TrimRight(sb.String(), " "))
	}
}

func padRight(s string, w int) string {
	n := dispWidth(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// termWidth 返回终端列宽（读取 COLUMNS 环境变量，缺省 80）。
func termWidth() int {
	if w := os.Getenv("COLUMNS"); w != "" {
		if n, err := strconv.Atoi(w); err == nil && n >= 20 {
			return n
		}
	}
	return 80
}
