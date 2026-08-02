package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestStressPushFolder 复现: 服务端推送大文件夹(大量小文件)时, 客户端并发拉取出现
// "file already closed" 与进度条卡住的问题。
func TestStressPushFolder(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	clientDir := filepath.Join(root, "client")
	pushDir := filepath.Join(dst, "pushdir")
	os.MkdirAll(pushDir, 0o755)
	os.MkdirAll(clientDir, 0o755)

	const nSmall = 8000
	for i := 0; i < nSmall; i++ {
		size := int64(1024 + (i%97)*300)
		payload := make([]byte, size)
		rand.Read(payload)
		p := filepath.Join(pushDir, fmt.Sprintf("f%04d.bin", i))
		if err := os.WriteFile(p, payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		payload := make([]byte, 2<<20+i*300000)
		rand.Read(payload)
		p := filepath.Join(pushDir, fmt.Sprintf("big%02d.bin", i))
		if err := os.WriteFile(p, payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	items, err := collectFiles([]string{pushDir})
	if err != nil {
		t.Fatal(err)
	}
	nFiles := len(items)

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	waitUser(t, sh, token)

	if !sh.execTp([]string{"tp", "pushdir", "1"}) {
		t.Fatal("tp 返回 false")
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	rcv := newReceiver(clientDir, "客户端", false)
	go rcv.progressLoop(ctx)
	go rcv.watchdog(ctx)
	go rcv.settleGroups(ctx)

	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []string
	)
	sem := make(chan struct{}, 4) // 与真实客户端一致: 并发拉取上限 = jobs
	for i := 0; i < nFiles; i++ {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		m := readCtrl(t, br)
		if m.Type != "pull" {
			t.Fatalf("意外消息: %+v", m)
		}
		wg.Add(1)
		go func(m ctrlMsg) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := pullFile(ctx, dialAddr(addr), token, rcv, m.Name, m.Size, m.Auth, 4, 3, false); err != nil {
				errMu.Lock()
				errs = append(errs, fmt.Sprintf("%s: %v", m.Name, err))
				errMu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	rcv.closeAll()

	if len(errs) > 0 {
		m := min(10, len(errs))
		t.Fatalf("%d 个文件拉取失败, 示例: %v", len(errs), errs[:m])
	}

	// 校验所有文件内容一致
	for _, it := range items {
		rel := filepath.FromSlash(it.rel)
		if !strings.HasPrefix(rel, "pushdir"+string(filepath.Separator)) {
			continue
		}
		got := sha256file(t, filepath.Join(clientDir, rel))
		want := sha256file(t, it.abs)
		if got != want {
			t.Fatalf("文件 %s 内容不一致", it.rel)
		}
	}
}