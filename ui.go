package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	outMu        sync.Mutex
	progressLive bool // 终端上是否有未换行的进度条
	promptShown  bool // 当前是否正在等待输入（提示符已显示）
)

// printLine 输出一行日志：先清掉进度行，若正在等待输入则重新打印提示符。
func printLine(format string, args ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	clearProgressLocked()
	if promptShown {
		fmt.Fprint(os.Stdout, "\n")
	}
	fmt.Fprintf(os.Stdout, format+"\n", args...)
	if promptShown {
		fmt.Fprint(os.Stdout, "> ")
	}
}

// prompt 在开始等待命令输入时打印提示符（总是另起一行，保证可读）。
func prompt() {
	outMu.Lock()
	defer outMu.Unlock()
	clearProgressLocked()
	fmt.Fprintln(os.Stdout)
	fmt.Fprint(os.Stdout, "> ")
	promptShown = true
}

// clearPromptFlag 收到指令开始执行时调用，避免输出日志时反复追加提示符。
func clearPromptFlag() {
	outMu.Lock()
	promptShown = false
	outMu.Unlock()
}

func clearProgressLocked() {
	if progressLive {
		fmt.Fprint(os.Stdout, "\r"+strings.Repeat(" ", 120)+"\r")
		progressLive = false
	}
}

// renderProgress 渲染一行进度：终端下用 \r 刷新，非终端只在 final 时输出一行。
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
	if promptShown && !progressLive {
		fmt.Fprint(os.Stdout, "\n")
	}
	fmt.Fprint(os.Stdout, line)
	if final {
		fmt.Fprintln(os.Stdout)
		progressLive = false
	} else {
		progressLive = true
	}
	if promptShown {
		fmt.Fprint(os.Stdout, "> ")
	}
}
