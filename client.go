package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fileItem struct {
	abs  string // 本地绝对路径
	rel  string // 发送给服务端的相对路径
	size int64
}

// collectFiles 展开 -f 参数：文件直接加入，目录递归展开。
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
			err := filepath.WalkDir(a, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
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

// runClient 连接服务端并发起传输，处理 -f 指定的全部文件/目录。
func runClient(ctx context.Context, addr string, files []string, proxy string, threads, retries int, verbose bool) error {
	items, err := collectFiles(files)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("没有可发送的文件")
	}
	var total int64
	for _, it := range items {
		total += it.size
	}
	start := time.Now()
	printLine("连接 %s", addr)
	for i, it := range items {
		printLine("[%d/%d] 发送 %s (%s)", i+1, len(items), it.rel, humanSize(it.size))
		if err := sendFile(ctx, addr, proxy, it, threads, retries, verbose); err != nil {
			return fmt.Errorf("%s: %w", it.rel, err)
		}
	}
	el := time.Since(start)
	rate := 0.0
	if el > 0 {
		rate = float64(total) / el.Seconds()
	}
	printLine("完成: %d 个文件, 共 %s, 用时 %s, 平均 %s/s", len(items), humanSize(total), el.Round(time.Millisecond), humanRate(rate))
	return nil
}

// sendFile 将单个文件拆块，通过多个并行 TCP 连接发送。
func sendFile(ctx context.Context, addr, proxy string, it fileItem, threads, retries int, verbose bool) error {
	f, err := os.Open(it.abs)
	if err != nil {
		return err
	}
	defer f.Close()

	chunks := chunkPlan(it.size, threads)
	id := randomID()
	prog := newProgress(it.rel, it.size)
	prog.start()

	ctxC, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(chunks))
	start := time.Now()

	for i, c := range chunks {
		wg.Add(1)
		go func(idx int, c chunk) {
			defer wg.Done()
			var lastErr error
			for attempt := 1; ; attempt++ {
				err := sendChunk(ctxC, addr, proxy, f, id, it.rel, it.size, idx, len(chunks), c, prog)
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
	prog.finish()

	var firstErr error
	for err := range errCh {
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	dur := time.Since(start)
	rate := 0.0
	if dur > 0 {
		rate = float64(it.size) / dur.Seconds()
	}
	printLine("已发送 %s (%s), 用时 %s, 平均 %s/s, %d 个并行连接",
		it.rel, humanSize(it.size), dur.Round(time.Millisecond), humanRate(rate), len(chunks))
	return nil
}

// sendChunk 通过一条连接发送一个分块：先写头部，再按偏移读取文件内容。
func sendChunk(ctx context.Context, addr, proxy string, f *os.File, id, name string, size int64, idx, total int, c chunk, prog *progress) error {
	conn, err := dialTarget(proxy, addr)
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

	h := chunkHeader{V: protoVersion, ID: id, Name: name, Size: size, Chunk: idx, Chunks: total, Start: c.start, Len: c.len}
	if err := writeHeader(conn, h); err != nil {
		return err
	}
	if c.len > 0 {
		sr := io.NewSectionReader(f, c.start, c.len)
		buf := make([]byte, 256*1024)
		if _, err := io.CopyBuffer(&countingWriter{w: conn, prog: prog}, sr, buf); err != nil {
			return err
		}
	}
	// 等待服务端确认该分块已落盘
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	return readAck(conn)
}

type countingWriter struct {
	w    io.Writer
	prog *progress
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.prog.add(int64(n))
	return n, err
}
