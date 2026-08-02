package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// transfer 是一次文件传输的状态（同一文件的所有分块共享）。
type transfer struct {
	id       string
	path     string
	name     string
	size     int64
	f        *os.File
	token    string // 所属会话令牌，用于用户断开时中止其传输
	total    int
	done     atomic.Int32
	last     atomic.Int64 // 最近活跃时间 (UnixNano)
	received atomic.Int64 // 已写入的字节数（重试会重复计数，展示时封顶）

	mu   sync.Mutex
	seen map[int]bool // 已接收的分块下标，保证重试幂等
}

// groupStat 统计一个顶层分组（单个文件或一个目录）的接收进度。
type groupStat struct {
	group    string
	dirLike  bool
	files    int
	bytes    int64
	active   int
	lastSeen time.Time
	reported bool
}

// groupOf 返回顶层分组名: 目录内文件取路径第一段, 单文件用整个文件名。
func groupOf(rel string) string {
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return rel[:i]
	}
	return rel
}

// receiver 是接收端引擎：负责分块落盘、进度渲染、目录日志合并。服务端与客户端共用。
type receiver struct {
	dir       string
	tag       string
	verbose   bool
	regMu     sync.Mutex
	reg       map[string]*transfer // 进行中的传输
	compMu    sync.Mutex
	completed map[string]time.Time // 已完成传输的 ID（用于重试幂等）
	grpMu     sync.Mutex
	groups    map[string]*groupStat
	aborted   atomic.Bool // 传输被中断（断开/停止），不再渲染"完成"行
}

func newReceiver(dir, tag string, verbose bool) *receiver {
	return &receiver{
		dir:       dir,
		tag:       tag,
		verbose:   verbose,
		reg:       make(map[string]*transfer),
		completed: make(map[string]time.Time),
		groups:    make(map[string]*groupStat),
	}
}

func (r *receiver) logf(format string, args ...any) {
	printLine("[%s] "+format, append([]any{r.tag}, args...)...)
}

// okf 输出一条绿色成功日志（接收完成等）。
func (r *receiver) okf(format string, args ...any) {
	printOK("[%s] "+format, append([]any{r.tag}, args...)...)
}

// handleChunkConn 处理一条分块连接（首部已由调用方解析）。
func (r *receiver) handleChunkConn(conn net.Conn, br *bufio.Reader, h chunkHeader) {
	defer conn.Close()
	if r.isCompleted(h.ID) {
		// 该传输已完成，重试的分块无需再写
		conn.Write([]byte(ackOK))
		return
	}
	tr, err := r.getTransfer(h)
	if err != nil {
		r.logf("传输 %s 失败: %v", h.Name, err)
		return
	}
	tr.last.Store(time.Now().UnixNano())
	if r.verbose {
		r.logf("接收分块 %d/%d: %s (偏移 %d, %s) 来自 %s", h.Chunk+1, h.Chunks, h.Name, h.Start, humanSize(h.Len), conn.RemoteAddr())
	}
	w := &offsetWriter{f: tr.f, off: h.Start, tr: tr}
	n, err := io.CopyN(w, br, h.Len)
	tr.last.Store(time.Now().UnixNano())
	if err != nil {
		r.logf("传输 %s 分块 %d 中断: %v (已写 %s)", h.Name, h.Chunk+1, err, humanSize(n))
		return
	}
	if r.chunkDone(tr, h.Chunk) {
		r.finishTransfer(tr)
	}
	conn.Write([]byte(ackOK))
}

func (r *receiver) getTransfer(h chunkHeader) (*transfer, error) {
	r.regMu.Lock()
	if tr, ok := r.reg[h.ID]; ok {
		r.regMu.Unlock()
		return tr, nil
	}
	r.regMu.Unlock()
	rel, err := sanitizeRelPath(h.Name)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(r.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	f, err := uniqueOpen(dest)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}
	tr := &transfer{id: h.ID, path: dest, name: rel, size: h.Size, f: f, token: h.User, total: h.Chunks, seen: make(map[int]bool)}
	tr.last.Store(time.Now().UnixNano()) // 立即激活，避免 watchdog 误杀尚未开始的分块
	r.regMu.Lock()
	if existing, ok := r.reg[h.ID]; ok {
		// 并发下另一个分块连接已创建同一传输，丢弃本次打开的文件
		r.regMu.Unlock()
		f.Close()
		return existing, nil
	}
	r.reg[h.ID] = tr
	r.regMu.Unlock()
	if r.verbose {
		r.logf("开始接收 %s (%s, %d 个分块)", tr.name, humanSize(tr.size), tr.total)
	} else {
		r.noteGroupStart(tr.name, humanSize(tr.size), tr.total)
	}
	return tr, nil
}

// chunkDone 记录已收到的分块；重复分块（重试）不再计数，返回 true 表示全部完成。
func (r *receiver) chunkDone(tr *transfer, idx int) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.seen[idx] {
		return false
	}
	tr.seen[idx] = true
	return tr.done.Add(1) == int32(tr.total)
}

func (r *receiver) finishTransfer(tr *transfer) {
	r.regMu.Lock()
	delete(r.reg, tr.id)
	r.regMu.Unlock()
	tr.f.Close()
	if fi, err := os.Stat(tr.path); err == nil && fi.Size() != tr.size {
		os.Truncate(tr.path, tr.size)
	}
	r.compMu.Lock()
	r.completed[tr.id] = time.Now()
	r.compMu.Unlock()
	if r.verbose {
		r.okf("接收完成 %s (%s)", tr.name, humanSize(tr.size))
		return
	}
	if !strings.Contains(tr.name, "/") {
		r.okf("接收完成 %s (%s)", tr.name, humanSize(tr.size))
		return
	}
	r.noteGroupDone(tr.name, tr.size)
}

// abortTransfer 中止一个未完成的传输：从注册表移除并关闭文件（不输出完成日志）。
func (r *receiver) abortTransfer(tr *transfer) {
	r.aborted.Store(true)
	r.regMu.Lock()
	if _, ok := r.reg[tr.id]; !ok {
		r.regMu.Unlock()
		return
	}
	delete(r.reg, tr.id)
	r.regMu.Unlock()
	tr.f.Close()
	r.grpMu.Lock()
	if g := r.groups[groupOf(tr.name)]; g != nil && g.active > 0 {
		g.active--
	}
	r.grpMu.Unlock()
}

// abortUser 中止属于指定会话令牌的所有未完成传输（用户断开时调用）。
func (r *receiver) abortUser(token string) {
	r.aborted.Store(true)
	var names []string
	r.regMu.Lock()
	for id, tr := range r.reg {
		if tr.token != token {
			continue
		}
		tr.f.Close()
		delete(r.reg, id)
		r.grpMu.Lock()
		if g := r.groups[groupOf(tr.name)]; g != nil && g.active > 0 {
			g.active--
		}
		r.grpMu.Unlock()
		names = append(names, tr.name)
	}
	r.regMu.Unlock()
	for _, n := range names {
		r.logf("用户断开，已中止未完成的传输: %s", n)
	}
}

func (r *receiver) isCompleted(id string) bool {
	r.compMu.Lock()
	defer r.compMu.Unlock()
	_, ok := r.completed[id]
	return ok
}

// snapshot 汇总当前进行中传输的接收进度。
func (r *receiver) snapshot() (received, size int64, name string, count int) {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	count = len(r.reg)
	if count == 0 {
		return 0, 0, "", 0
	}
	for _, tr := range r.reg {
		v := tr.received.Load()
		if v > tr.size {
			v = tr.size
		}
		received += v
		size += tr.size
		if count == 1 {
			name = tr.name
		}
	}
	return received, size, name, count
}

// progressLoop 渲染接收端进度：终端下用 \r 在同一行内刷新；非终端只在结束时输出一行。
func (r *receiver) progressLoop(ctx context.Context) {
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
		received, size, name, count := r.snapshot()
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
			r.renderProgress(received, size, name, count, rate, false)
			wasActive = true
		} else if wasActive {
			if r.aborted.Load() {
				// 传输被中断（断开/停止）：清掉进度条，不输出误导性的完成行
				clearProgressNow()
			} else {
				// 全部传输完成，输出最终一行
				r.renderProgress(lastSize, lastSize, lastName, 1, rate, true)
			}
			wasActive = false
			prevTotal, rate = 0, 0
			r.aborted.Store(false)
		}
	}
}

func (r *receiver) renderProgress(received, size int64, name string, count int, rate float64, final bool) {
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
	renderProgress(line, final)
}

// noteGroupStart 记录一个文件开始接收: 单文件保留原逐文件日志, 目录文件只输出一行。
func (r *receiver) noteGroupStart(name, sizeStr string, chunks int) {
	dirLike := strings.Contains(name, "/")
	r.grpMu.Lock()
	defer r.grpMu.Unlock()
	if !dirLike {
		r.logf("开始接收 %s (%s, %d 个分块)", name, sizeStr, chunks)
		return
	}
	group := groupOf(name)
	g := r.groups[group]
	if g == nil {
		// 新目录: 先结算已完成的其它目录, 再输出本目录的开始行
		for og, o := range r.groups {
			if og != group && o.files > 0 && o.active == 0 && !o.reported {
				r.reportGroupLocked(o)
				delete(r.groups, og)
			}
		}
		g = &groupStat{group: group, dirLike: true}
		r.groups[group] = g
		r.logf("开始接收 %s (目录, 多个文件)", group)
	}
	g.active++
	g.lastSeen = time.Now()
}

// noteGroupDone 记录一个文件接收完成, 并汇总到所属目录统计。
func (r *receiver) noteGroupDone(name string, size int64) {
	group := groupOf(name)
	r.grpMu.Lock()
	defer r.grpMu.Unlock()
	g := r.groups[group]
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
func (r *receiver) reportGroupLocked(g *groupStat) {
	g.reported = true
	msg := fmt.Sprintf("接收完成 %s (%d 个文件, 共 %s)", g.group, g.files, humanSize(g.bytes))
	if strings.HasPrefix(g.group, ".") {
		msg += " (以 . 开头是隐藏目录, 在保存目录下用 ls -a 查看)"
	}
	r.okf("%s", msg)
}

// finalizeIdleGroups 结算空闲一段时间的目录。
func (r *receiver) finalizeIdleGroups(idle time.Duration) {
	r.grpMu.Lock()
	defer r.grpMu.Unlock()
	cutoff := time.Now().Add(-idle)
	for group, g := range r.groups {
		if g.reported || g.files == 0 || g.active > 0 {
			continue
		}
		if g.lastSeen.Before(cutoff) {
			r.reportGroupLocked(g)
			delete(r.groups, group)
		}
	}
}

// finalizeAllGroups 停止时结算所有未输出的目录汇总。
func (r *receiver) finalizeAllGroups() {
	r.grpMu.Lock()
	defer r.grpMu.Unlock()
	for group, g := range r.groups {
		if g.files > 0 && !g.reported {
			r.reportGroupLocked(g)
		}
		delete(r.groups, group)
	}
}

// settleGroups 定时结算空闲目录, 退出时输出剩余汇总。
func (r *receiver) settleGroups(ctx context.Context) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.finalizeAllGroups()
			return
		case <-t.C:
			r.finalizeIdleGroups(2 * time.Second)
		}
	}
}

// watchdog 清理超时未完成的传输（发送方放弃重试后 15 秒内回收）。
func (r *receiver) watchdog(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-60 * time.Second).UnixNano()
			r.regMu.Lock()
			for id, tr := range r.reg {
				if tr.last.Load() < cutoff {
					tr.f.Close()
					delete(r.reg, id)
					r.grpMu.Lock()
					if g := r.groups[groupOf(tr.name)]; g != nil && g.active > 0 {
						g.active--
					}
					r.grpMu.Unlock()
					r.aborted.Store(true)
					r.logf("清理超时未完成的传输: %s", tr.name)
				}
			}
			r.regMu.Unlock()
			compCutoff := time.Now().Add(-1 * time.Hour)
			r.compMu.Lock()
			for id, at := range r.completed {
				if at.Before(compCutoff) {
					delete(r.completed, id)
				}
			}
			r.compMu.Unlock()
		}
	}
}

// closeAll 关闭所有进行中的传输并输出剩余汇总。
func (r *receiver) closeAll() {
	r.aborted.Store(true)
	r.regMu.Lock()
	for id, tr := range r.reg {
		tr.f.Close()
		delete(r.reg, id)
	}
	r.regMu.Unlock()
	r.finalizeAllGroups()
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
		w.tr.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// fileItem 是一个待发送的本地文件。
type fileItem struct {
	abs  string // 本地绝对路径
	rel  string // 发送给接收端的相对路径
	size int64
}

// collectFiles 展开路径参数：文件直接加入，目录递归展开（保留顶层文件夹名）。
func collectFiles(args []string) ([]fileItem, error) {
	var items []fileItem
	for _, a := range args {
		fi, err := os.Stat(a)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", a, err)
		}
		if fi.IsDir() {
			base := filepath.Base(a)
			if base == "." || base == string(filepath.Separator) {
				base = filepath.Base(filepath.Clean(a))
			}
			err := filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(a, p)
				if err != nil {
					return err
				}
				items = append(items, fileItem{abs: p, rel: filepath.ToSlash(filepath.Join(base, rel)), size: info.Size()})
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a, err)
			}
		} else {
			items = append(items, fileItem{abs: a, rel: filepath.Base(a), size: fi.Size()})
		}
	}
	return items, nil
}

// sendItems 把多个文件/目录拆块并发生送（发送端）。dial 负责建立到接收端的分块连接。
func sendItems(ctx context.Context, dial func() (net.Conn, error), token string, items []fileItem, threads, retries, jobs int, verbose bool) error {
	if len(items) == 0 {
		return fmt.Errorf("没有可发送的文件")
	}
	if jobs < 1 {
		jobs = 1
	}
	var total int64
	for _, it := range items {
		total += it.size
	}
	bp := newBatchProgress(len(items), total)
	bp.start()

	ctxC, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, jobs)
loop:
	for _, it := range items {
		select {
		case <-ctxC.Done():
			break loop
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it fileItem) {
			defer wg.Done()
			defer func() { <-sem }()
			bp.setName(it.rel)
			if err := sendFile(ctxC, dial, token, it, threads, retries, verbose, bp); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", it.rel, err)
				}
				errMu.Unlock()
				return
			}
			bp.fileDone()
		}(it)
	}
	wg.Wait()
	bp.finish()
	return firstErr
}

// sendFile 将单个文件拆块，通过多个并行 TCP 连接发送。
func sendFile(ctx context.Context, dial func() (net.Conn, error), token string, it fileItem, threads, retries int, verbose bool, bp *batchProgress) error {
	f, err := os.Open(it.abs)
	if err != nil {
		return err
	}
	defer f.Close()

	chunks := chunkPlan(it.size, threads)
	id := randomID()

	ctxC, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(chunks))

	for i, c := range chunks {
		wg.Add(1)
		go func(idx int, c chunk) {
			defer wg.Done()
			var lastErr error
			for attempt := 1; ; attempt++ {
				err := sendChunk(ctxC, dial, token, f, id, it.rel, it.size, idx, len(chunks), c, bp)
				if err == nil {
					return
				}
				lastErr = err
				if attempt > retries {
					break
				}
				if verbose {
					printLine("  分块 %d/%d 第 %d 次尝试失败: %v", idx+1, len(chunks), attempt, err)
				}
				select {
				case <-ctxC.Done():
					return
				case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
				}
			}
			if lastErr != nil {
				select {
				case errCh <- fmt.Errorf("分块 %d/%d: %w", idx+1, len(chunks), lastErr):
				default:
				}
			}
		}(i, c)
	}
	wg.Wait()
	close(errCh)

	var firstErr error
	for err := range errCh {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// sendChunk 通过一条连接发送一个分块：先写头部，再按偏移读取文件内容。
func sendChunk(ctx context.Context, dial func() (net.Conn, error), token string, f *os.File, id, name string, size int64, idx, total int, c chunk, bp *batchProgress) error {
	conn, err := dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	h := chunkHeader{V: protoVersion, ID: id, User: token, Name: name, Size: size, Chunk: idx, Chunks: total, Start: c.start, Len: c.len}
	if err := writeHeader(conn, h); err != nil {
		return err
	}
	if c.len > 0 {
		sr := io.NewSectionReader(f, c.start, c.len)
		buf := make([]byte, 256*1024)
		if _, err := io.CopyBuffer(&countingWriter{w: conn, bp: bp}, sr, buf); err != nil {
			return err
		}
	}
	// 等待接收端确认该分块已落盘
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	return readAck(conn)
}

type countingWriter struct {
	w  io.Writer
	bp *batchProgress
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.bp.add(int64(n))
	return n, err
}

// pullFile 从服务端拉取一个文件（客户端主动发起分块连接，方向为服务端 -> 客户端）。
// 文件保存路径由 rcv 决定，进度复用接收引擎渲染。
func pullFile(ctx context.Context, dial func() (net.Conn, error), token string, rcv *receiver, name string, size int64, auth string, threads, retries int, verbose bool) error {
	chunks := chunkPlan(size, threads)
	id := randomID()
	h := chunkHeader{V: protoVersion, ID: id, User: token, Name: name, Size: size, Chunks: len(chunks)}
	tr, err := rcv.getTransfer(h)
	if err != nil {
		return err
	}

	ctxC, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for i, c := range chunks {
		wg.Add(1)
		go func(idx int, c chunk) {
			defer wg.Done()
			var lastErr error
			for attempt := 1; ; attempt++ {
				err := pullChunk(ctxC, dial, token, tr, idx, c, auth)
				if err == nil {
					// 与上传接收一致：每个成功分块计数一次，最后一个触发完成
					if rcv.chunkDone(tr, idx) {
						rcv.finishTransfer(tr)
					}
					return
				}
				lastErr = err
				if attempt > retries {
					break
				}
				if verbose {
					printLine("  分块 %d/%d 第 %d 次尝试失败: %v", idx+1, len(chunks), attempt, err)
				}
				select {
				case <-ctxC.Done():
					return
				case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
				}
			}
			if lastErr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("分块 %d/%d: %w", idx+1, len(chunks), lastErr)
				}
				errMu.Unlock()
			}
		}(i, c)
	}
	wg.Wait()
	if firstErr != nil {
		rcv.abortTransfer(tr)
		return firstErr
	}
	return nil
}

// pullChunk 通过一条连接拉取一个分块：先写头部，再读取服务端写出的数据并落盘。
func pullChunk(ctx context.Context, dial func() (net.Conn, error), token string, tr *transfer, idx int, c chunk, auth string) error {
	conn, err := dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	h := chunkHeader{V: protoVersion, ID: tr.id, User: token, Name: tr.name, Size: tr.size, Chunk: idx, Chunks: tr.total, Start: c.start, Len: c.len, Dir: chunkDirOut, Auth: auth}
	if err := writeHeader(conn, h); err != nil {
		return err
	}
	tr.last.Store(time.Now().UnixNano())
	if c.len > 0 {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		w := &offsetWriter{f: tr.f, off: c.start, tr: tr}
		if _, err := io.CopyN(w, conn, c.len); err != nil {
			return err
		}
	}
	tr.last.Store(time.Now().UnixNano())
	return nil
}
