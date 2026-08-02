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
	if progressLive || (colorOn && (promptShown || editActive)) {
		fmt.Fprint(os.Stdout, "\r"+strings.Repeat(" ", 120)+"\r")
	}
	progressLive = false
	promptShown = false
	promptOnScreen = false
	promptDirty = false
	editActive = false
}

// drawInputLocked 重绘提示行（进度条存在时一并重绘），调用方需持有 outMu。
// 整行拼成一次写入，减少逐字节刷屏带来的输入卡顿。
func drawInputLocked() {
	var sb strings.Builder
	sb.WriteString("\r\x1b[2K")
	if progressLive && lastBarLine != "" {
		sb.WriteString(lastBarLine)
		sb.WriteString(" ")
	}
	sb.WriteString(paint(promptColor, "> "))
	if editActive {
		sb.WriteString(string(editBuf))
		if editPos < runeLen(string(editBuf)) {
			fmt.Fprintf(&sb, "\x1b[%dD", runeLen(string(editBuf))-editPos)
		}
	}
	fmt.Fprint(os.Stdout, sb.String())
	promptOnScreen = true
}

func clearProgressLocked() {
	if progressLive {
		fmt.Fprint(os.Stdout, "\r"+strings.Repeat(" ", 120)+"\r")
		progressLive = false
		promptOnScreen = false
	}
}

// renderProgress 渲染一行进度：进度条与提示符共占一行（终端下用 \r 刷新），
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
	if promptShown {
		// 行尾保留提示符（含正在编辑的缓冲），方便传输期间继续输入
		fmt.Fprint(os.Stdout, paint(promptColor, "> "))
		if editActive {
			fmt.Fprint(os.Stdout, string(editBuf))
			if editPos < runeLen(string(editBuf)) {
				fmt.Fprintf(os.Stdout, "\x1b[%dD", runeLen(string(editBuf))-editPos)
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
		if w := runeLen(e); w > maxW {
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
	n := runeLen(s)
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
