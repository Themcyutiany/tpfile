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

// batchProgress 负责发送端整体进度渲染：多个文件并行发送时，所有进度合并到一行实时刷新。
// 终端下用 \r 在同一行内刷新；非终端（重定向/日志）只在结束时输出一行汇总。
type batchProgress struct {
	total   int64
	totalN  int
	done    atomic.Int64
	doneN   atomic.Int64
	curName atomic.Value // string
	startAt time.Time
	tty     bool
	stop    chan struct{}
	doneC   chan struct{}

	lastDone int64
	lastAt   time.Time
	rate     float64
}

func newBatchProgress(totalN int, total int64) *batchProgress {
	p := &batchProgress{
		total:   total,
		totalN:  totalN,
		startAt: time.Now(),
		tty:     isTerminal(os.Stdout),
		stop:    make(chan struct{}),
		doneC:   make(chan struct{}),
	}
	p.curName.Store("")
	return p
}

func (p *batchProgress) add(n int64)         { p.done.Add(n) }
func (p *batchProgress) fileDone()           { p.doneN.Add(1) }
func (p *batchProgress) setName(name string) { p.curName.Store(name) }

func (p *batchProgress) start() { go p.loop() }

// finish 停止渲染并输出最终一行。
func (p *batchProgress) finish() {
	close(p.stop)
	<-p.doneC
	p.render(true)
}

func (p *batchProgress) loop() {
	defer close(p.doneC)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
			if p.tty {
				p.render(false)
			}
		}
	}
}

func (p *batchProgress) render(final bool) {
	done := p.done.Load()
	pct := 0.0
	if p.total > 0 {
		pct = float64(done) * 100 / float64(p.total)
		if pct > 100 {
			pct = 100
		}
	}
	now := time.Now()
	if !p.lastAt.IsZero() && now.After(p.lastAt) {
		inst := float64(done-p.lastDone) / now.Sub(p.lastAt).Seconds()
		if inst < 0 {
			inst = 0
		}
		if p.rate == 0 {
			p.rate = inst
		} else {
			p.rate = p.rate*0.7 + inst*0.3
		}
	}
	p.lastDone, p.lastAt = done, now
	rate := p.rate

	eta := "--:--"
	remaining := p.total - done
	if remaining < 0 {
		remaining = 0
	}
	if rate > 0 && remaining > 0 {
		eta = etaStr(time.Duration(float64(remaining) / rate * float64(time.Second)))
	}
	cur := p.doneN.Load()
	if cur < int64(p.totalN) {
		cur++
	}
	name, _ := p.curName.Load().(string)
	line := fmt.Sprintf("[%d/%d] %5.1f%% %s %9s/s 剩余 %s (%s/%s) %s",
		cur, p.totalN, pct, bar(pct, 20), humanRate(rate), eta, humanSize(done), humanSize(p.total), truncate(name, 28))

	renderProgress(line, final)
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
