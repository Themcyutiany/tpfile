package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type transfer struct {
	id       string
	path     string
	name     string
	size     int64
	f        *os.File
	total    int
	done     atomic.Int32
	last     atomic.Int64 // 最近活跃时间 (UnixNano)
	received atomic.Int64 // 已写入的字节数（重试会重复计数，展示时封顶）

	mu   sync.Mutex
	seen map[int]bool // 已接收的分块下标，保证重试幂等
}

type server struct {
	dir       string
	regMu     sync.Mutex
	reg       map[string]*transfer // 进行中的传输
	compMu    sync.Mutex
	completed map[string]time.Time // 已完成传输的 ID（用于重试幂等）
	grpMu     sync.Mutex
	groups    map[string]*groupStat // 顶层目录/单文件的汇总统计
}

// groupStat 统计一个顶层分组（单个文件或一个目录）的接收进度。
type groupStat struct {
	group    string    // 顶层名称: 目录的第一段, 或单个文件名
	dirLike  bool      // 是否为目录内文件
	files    int       // 已完成的文件数
	bytes    int64     // 已完成字节数
	active   int       // 正在接收的文件数（并行/多客户端时防止提前结算）
	lastSeen time.Time // 最近一次有文件到达的时间
	reported bool      // 是否已输出汇总
}

// groupOf 返回顶层分组名: 目录内文件取路径第一段, 单文件用整个文件名。
func groupOf(rel string) string {
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return rel[:i]
	}
	return rel
}

func newServer(dir string) *server {
	return &server{dir: dir, reg: make(map[string]*transfer), completed: make(map[string]time.Time), groups: make(map[string]*groupStat)}
}

func (s *server) logf(format string, args ...any) {
	printLine("[服务端] "+format, args...)
}

// runServer 监听并接收文件，直到 ctx 被取消。
func runServer(ctx context.Context, port int, dir string, verbose bool) error {
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建保存目录失败: %w", err)
	}

	addr := net.JoinHostPort("::", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	mode := "IPv4+IPv6"
	if err != nil {
		// 部分系统未开启双栈时退回 IPv4
		ln, err = net.Listen("tcp", ":"+strconv.Itoa(port))
		mode = "IPv4"
		if err != nil {
			return fmt.Errorf("监听端口 %d 失败: %w", port, err)
		}
	}

	s := newServer(dir)
	go s.watchdog(ctx)
	go s.progressLoop(ctx)
	go s.settleGroups(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
		s.closeAll()
	}()

	s.logf("已启动，监听 %s (%s)", ln.Addr(), mode)
	s.logf("接收的文件将保存到: %s", dir)
	s.logf("提示: 以 . 开头的目录(如 .minecraft)在 Linux 下默认隐藏, 用 ls -a 查看")

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn, verbose)
	}
}

func (s *server) handleConn(ctx context.Context, conn net.Conn, verbose bool) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)
	h, err := readHeader(br)
	if err != nil {
		s.logf("来自 %s 的连接头部无效: %v", conn.RemoteAddr(), err)
		return
	}
	if s.isCompleted(h.ID) {
		// 该传输已完成，重试的分块无需再写
		conn.Write([]byte(ackOK))
		return
	}
	tr, err := s.getTransfer(h, verbose)
	if err != nil {
		s.logf("传输 %s 失败: %v", h.Name, err)
		return
	}
	tr.last.Store(time.Now().UnixNano())
	if verbose {
		s.logf("接收分块 %d/%d: %s (偏移 %d, %s) 来自 %s", h.Chunk+1, h.Chunks, h.Name, h.Start, humanSize(h.Len), conn.RemoteAddr())
	}

	w := &offsetWriter{f: tr.f, off: h.Start, tr: tr}
	n, err := io.CopyN(w, br, h.Len)
	tr.last.Store(time.Now().UnixNano())
	if err != nil {
		s.logf("传输 %s 分块 %d 中断: %v (已写 %s)", h.Name, h.Chunk+1, err, humanSize(n))
		return
	}
	if s.chunkDone(tr, h.Chunk) {
		s.finishTransfer(tr, verbose)
	}
	conn.Write([]byte(ackOK))
}

func (s *server) getTransfer(h chunkHeader, verbose bool) (*transfer, error) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if tr, ok := s.reg[h.ID]; ok {
		return tr, nil
	}
	rel, err := sanitizeRelPath(h.Name)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	f, err := uniqueOpen(dest)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}
	tr := &transfer{id: h.ID, path: dest, name: rel, size: h.Size, f: f, total: h.Chunks, seen: make(map[int]bool)}
	s.reg[h.ID] = tr
	if verbose {
		s.logf("开始接收 %s (%s, %d 个分块)", tr.name, humanSize(tr.size), tr.total)
	} else {
		s.noteGroupStart(tr.name, humanSize(tr.size), tr.total)
	}
	return tr, nil
}

// chunkDone 记录已收到的分块；重复分块（重试）不再计数，返回 true 表示全部完成。
func (s *server) chunkDone(tr *transfer, idx int) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.seen[idx] {
		return false
	}
	tr.seen[idx] = true
	return tr.done.Add(1) == int32(tr.total)
}

func (s *server) finishTransfer(tr *transfer, verbose bool) {
	s.regMu.Lock()
	delete(s.reg, tr.id)
	s.regMu.Unlock()
	tr.f.Close()
	if fi, err := os.Stat(tr.path); err == nil && fi.Size() != tr.size {
		os.Truncate(tr.path, tr.size)
	}
	s.compMu.Lock()
	s.completed[tr.id] = time.Now()
	s.compMu.Unlock()
	if verbose {
		s.logf("接收完成 %s (%s)", tr.name, humanSize(tr.size))
		return
	}
	if !strings.Contains(tr.name, "/") {
		s.logf("接收完成 %s (%s)", tr.name, humanSize(tr.size))
		return
	}
	s.noteGroupDone(tr.name, tr.size)
}

// noteGroupStart 记录一个文件开始接收: 单文件保留原逐文件日志, 目录文件只输出一行。
func (s *server) noteGroupStart(name, sizeStr string, chunks int) {
	dirLike := strings.Contains(name, "/")
	s.grpMu.Lock()
	defer s.grpMu.Unlock()
	if !dirLike {
		s.logf("开始接收 %s (%s, %d 个分块)", name, sizeStr, chunks)
		return
	}
	group := groupOf(name)
	g := s.groups[group]
	if g == nil {
		// 新目录: 先结算已完成的其它目录, 再输出本目录的开始行
		for og, o := range s.groups {
			if og != group && o.files > 0 && o.active == 0 && !o.reported {
				s.reportGroupLocked(o)
				delete(s.groups, og)
			}
		}
		g = &groupStat{group: group, dirLike: true}
		s.groups[group] = g
		s.logf("开始接收 %s (目录, 多个文件)", group)
	}
	g.active++
	g.lastSeen = time.Now()
}

// noteGroupDone 记录一个文件接收完成, 并汇总到所属目录统计。
func (s *server) noteGroupDone(name string, size int64) {
	group := groupOf(name)
	s.grpMu.Lock()
	defer s.grpMu.Unlock()
	g := s.groups[group]
	if g == nil {
		return
	}
	g.files++
	g.bytes += size
	g.active--
	if g.active < 0 {
		g.active = 0
	}
	g.lastSeen = time.Now()
}

// reportGroupLocked 输出一个目录接收汇总（调用方需持有 grpMu）。
func (s *server) reportGroupLocked(g *groupStat) {
	g.reported = true
	msg := fmt.Sprintf("接收完成 %s (%d 个文件, 共 %s)", g.group, g.files, humanSize(g.bytes))
	if strings.HasPrefix(g.group, ".") {
		msg += " (以 . 开头是隐藏目录, 在保存目录下用 ls -a 查看)"
	}
	s.logf("%s", msg)
}

// finalizeIdleGroups 结算空闲一段时间的目录。
func (s *server) finalizeIdleGroups(idle time.Duration) {
	s.grpMu.Lock()
	defer s.grpMu.Unlock()
	cutoff := time.Now().Add(-idle)
	for group, g := range s.groups {
		if g.reported || g.files == 0 || g.active > 0 {
			continue
		}
		if g.lastSeen.Before(cutoff) {
			s.reportGroupLocked(g)
			delete(s.groups, group)
		}
	}
}

// finalizeAllGroups 服务停止时结算所有未输出的目录汇总。
func (s *server) finalizeAllGroups() {
	s.grpMu.Lock()
	defer s.grpMu.Unlock()
	for group, g := range s.groups {
		if g.files > 0 && !g.reported {
			s.reportGroupLocked(g)
		}
		delete(s.groups, group)
	}
}

// settleGroups 定时结算空闲目录, 服务退出时输出剩余汇总。
func (s *server) settleGroups(ctx context.Context) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finalizeAllGroups()
			return
		case <-t.C:
			s.finalizeIdleGroups(2 * time.Second)
		}
	}
}

func (s *server) isCompleted(id string) bool {
	s.compMu.Lock()
	defer s.compMu.Unlock()
	_, ok := s.completed[id]
	return ok
}

// snapshot 汇总当前进行中传输的接收进度。
func (s *server) snapshot() (received, size int64, name string, count int) {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	count = len(s.reg)
	if count == 0 {
		return 0, 0, "", 0
	}
	for _, tr := range s.reg {
		r := tr.received.Load()
		if r > tr.size {
			r = tr.size
		}
		received += r
		size += tr.size
		if count == 1 {
			name = tr.name
		}
	}
	return received, size, name, count
}

// progressLoop 渲染服务端接收进度条：终端下用 \r 在同一行内刷新；非终端（重定向/日志）只在结束时输出一行汇总。
func (s *server) progressLoop(ctx context.Context) {
	tty := isTerminal(os.Stdout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var (
		prevTotal int64
		prevTime  time.Time
		rate      float64
		wasActive bool
		lastName  string
		lastSize  int64
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		received, size, name, count := s.snapshot()
		now := time.Now()
		if count > 0 {
			if !wasActive {
				prevTotal, prevTime, rate = 0, now, 0
			}
			if !prevTime.IsZero() && now.After(prevTime) {
				inst := float64(received-prevTotal) / now.Sub(prevTime).Seconds()
				if inst < 0 {
					inst = 0
				}
				if rate == 0 {
					rate = inst
				} else {
					rate = rate*0.7 + inst*0.3
				}
			}
			prevTotal, prevTime = received, now
			lastName, lastSize = name, size
			s.renderProgress(tty, received, size, name, count, rate, false)
			wasActive = true
		} else if wasActive {
			// 全部传输完成，输出最终一行（此时必然已全部接收）
			s.renderProgress(tty, lastSize, lastSize, lastName, 1, rate, true)
			wasActive = false
			prevTotal, rate = 0, 0
		}
	}
}

func (s *server) renderProgress(tty bool, received, size int64, name string, count int, rate float64, final bool) {
	pct := 0.0
	if size > 0 {
		pct = float64(received) * 100 / float64(size)
		if pct > 100 {
			pct = 100
		}
	}
	remaining := size - received
	if remaining < 0 {
		remaining = 0
	}
	eta := "--:--"
	if rate > 0 && remaining > 0 {
		eta = etaStr(time.Duration(float64(remaining) / rate * float64(time.Second)))
	}
	label := name
	if count > 1 {
		label = fmt.Sprintf("%d 个传输", count)
	}
	line := fmt.Sprintf("接收中 %5.1f%% %s %9s/s 剩余 %s (%s/%s) %s",
		pct, bar(pct, 20), humanRate(rate), eta, humanSize(received), humanSize(size), truncate(label, 16))

	outMu.Lock()
	defer outMu.Unlock()
	if tty {
		fmt.Fprintf(os.Stdout, "\r%-95s", line)
		if final {
			progressLive = false
			fmt.Fprintln(os.Stdout)
		} else {
			progressLive = true
		}
	} else if final {
		fmt.Fprintln(os.Stdout, line)
	}
}

func (s *server) watchdog(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-30 * time.Minute).UnixNano()
			s.regMu.Lock()
			for id, tr := range s.reg {
				if tr.last.Load() < cutoff {
					tr.f.Close()
					delete(s.reg, id)
					s.grpMu.Lock()
					if g := s.groups[groupOf(tr.name)]; g != nil && g.active > 0 {
						g.active--
					}
					s.grpMu.Unlock()
					s.logf("清理超时未完成的传输: %s", tr.name)
				}
			}
			s.regMu.Unlock()
			compCutoff := time.Now().Add(-1 * time.Hour)
			s.compMu.Lock()
			for id, at := range s.completed {
				if at.Before(compCutoff) {
					delete(s.completed, id)
				}
			}
			s.compMu.Unlock()
		}
	}
}

func (s *server) closeAll() {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	for id, tr := range s.reg {
		tr.f.Close()
		delete(s.reg, id)
	}
	s.finalizeAllGroups()
}

// offsetWriter 按指定偏移写入同一文件句柄，各分块并发安全，并累计已写入字节。
type offsetWriter struct {
	f   *os.File
	off int64
	tr  *transfer
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	if w.tr != nil {
		w.tr.received.Add(int64(n))
	}
	return n, err
}
