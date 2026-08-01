package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// isTerminal 判断标准输出是否为终端。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// progress 负责客户端进度条渲染：终端下用 \r 覆盖刷新，非终端（重定向/日志）下按固定间隔输出一行。
type progress struct {
	name    string
	size    int64
	done    atomic.Int64
	startAt time.Time
	tty     bool
	stop    chan struct{}
	doneC   chan struct{}
}

func newProgress(name string, size int64) *progress {
	return &progress{
		name:    name,
		size:    size,
		startAt: time.Now(),
		tty:     isTerminal(os.Stdout),
		stop:    make(chan struct{}),
		doneC:   make(chan struct{}),
	}
}

func (p *progress) add(n int64) { p.done.Add(n) }

func (p *progress) start() { go p.loop() }

// finish 停止渲染并输出最终一行。
func (p *progress) finish() {
	close(p.stop)
	<-p.doneC
	p.render(true)
}

func (p *progress) loop() {
	defer close(p.doneC)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
			n++
			if !p.tty && n%20 != 0 {
				continue
			}
			p.render(false)
		}
	}
}

func (p *progress) render(final bool) {
	done := p.done.Load()
	pct := 0.0
	if p.size > 0 {
		pct = float64(done) * 100 / float64(p.size)
		if pct > 100 {
			pct = 100
		}
	}
	el := time.Since(p.startAt).Seconds()
	rate := 0.0
	if el > 0 {
		rate = float64(done) / el
	}
	eta := "--:--"
	if rate > 0 && done < p.size {
		eta = etaStr(time.Duration(float64(p.size-done) / rate * float64(time.Second)))
	}
	line := fmt.Sprintf("%-24s %5.1f%% %s %9s/s 剩余 %s  (%s/%s)",
		truncate(p.name, 24), pct, bar(pct, 28), humanRate(rate), eta, humanSize(done), humanSize(p.size))

	outMu.Lock()
	defer outMu.Unlock()
	if p.tty {
		fmt.Fprintf(os.Stdout, "\r%-112s", line)
		if final {
			progressLive = false
			fmt.Fprintln(os.Stdout)
		} else {
			progressLive = true
		}
	} else {
		fmt.Fprintln(os.Stdout, line)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-2]) + ".."
}

func bar(pct float64, width int) string {
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}
