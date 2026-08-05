package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResumeBitsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	name := "sub/档案.bin"
	if err := saveResumeBits(dir, name, 100, []bool{true, false, true}); err != nil {
		t.Fatal(err)
	}
	bits, ok := loadResumeBits(dir, name, 100)
	if !ok || len(bits) != 3 || !bits[0] || bits[1] || !bits[2] {
		t.Fatalf("位图回读异常: %v %v", bits, ok)
	}
	if _, ok := loadResumeBits(dir, name, 99); ok {
		t.Fatal("大小不匹配应失效")
	}
	clearResumeBits(dir, name)
	if _, ok := loadResumeBits(dir, name, 100); ok {
		t.Fatal("清除后应不存在")
	}
}

func TestSanitizeResume(t *testing.T) {
	// 4 个 512KB 分块，文件只写了前 1MB：chunk0/1 有效，chunk2/3 失效
	bits := sanitizeResume([]bool{true, true, true, true}, 2<<20, 1<<20)
	want := []bool{true, true, false, false}
	for i := range want {
		if bits[i] != want[i] {
			t.Fatalf("sanitizeResume[%d] = %v, want %v", i, bits[i], want)
		}
	}
}

// TestResumeUpload 验证上传方向断点续传：只发送缺失分块，完成后清理位图。
func TestResumeUpload(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	payload := make([]byte, 2<<20) // 2MB → 4 分块 × 512KB
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(root, "big.bin")
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	_, cancel := startTestServer(t, port, dst)
	defer cancel()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()

	// 模拟上次中断：服务端已写入前 1MB（chunk0/1），位图标记这两个分块完成
	partial := filepath.Join(dst, "big.bin")
	if err := os.WriteFile(partial, payload[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveResumeBits(dst, "big.bin", int64(len(payload)), []bool{true, true, false, false}); err != nil {
		t.Fatal(err)
	}

	items, err := collectFiles([]string{srcFile})
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	dial := func() (net.Conn, error) {
		dials.Add(1)
		return net.DialTimeout("tcp", addr, 5*time.Second)
	}
	if err := sendItems(context.Background(), dial, token, items, 4, 2, 4, false); err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	if got := sha256file(t, partial); got != sha256file(t, srcFile) {
		t.Fatal("续传后文件内容不一致")
	}
	if n := dials.Load(); n != 3 { // 1 次续传查询 + 2 个缺失分块
		t.Fatalf("应只建立 3 条连接(1 查询+2 分块), 实际 %d", n)
	}
	if _, err := os.Stat(resumeFilePath(dst, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("完成后位图应被删除")
	}
}

// TestResumePull 验证拉取方向断点续传：只拉取缺失分块，完成后清理位图。
func TestResumePull(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(root, "big.bin")
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	waitUser(t, sh, token)

	// 服务端签发一个拉取授权
	auth := randomID()
	push := &pushJob{id: randomID(), totalN: 1, bp: newBatchProgress(1, int64(len(payload))), expires: time.Now().Add(pullAuthTTL), user: 1}
	sh.pullMu.Lock()
	sh.pulls[auth] = &pullAuth{abs: srcFile, size: int64(len(payload)), name: "big.bin", push: push, added: time.Now(), user: 1}
	sh.pullMu.Unlock()

	// 客户端本地已有前 1MB + 位图
	clientDir := filepath.Join(root, "client")
	os.MkdirAll(clientDir, 0o755)
	partial := filepath.Join(clientDir, "big.bin")
	if err := os.WriteFile(partial, payload[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveResumeBits(clientDir, "big.bin", int64(len(payload)), []bool{true, true, false, false}); err != nil {
		t.Fatal(err)
	}

	ctx, cctx := context.WithCancel(context.Background())
	defer cctx()
	rcv := newReceiver(clientDir, "客户端", false)
	go rcv.progressLoop(ctx)
	go rcv.watchdog(ctx)
	go rcv.settleGroups(ctx)
	defer rcv.closeAll()

	if err := pullFile(ctx, dialAddr(addr), token, rcv, "big.bin", int64(len(payload)), auth, 4, 2, false); err != nil {
		t.Fatalf("拉取续传失败: %v", err)
	}
	if got := sha256file(t, partial); got != sha256file(t, srcFile) {
		t.Fatal("拉取续传后文件内容不一致")
	}
	if _, err := os.Stat(resumeFilePath(clientDir, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("完成后位图应被删除")
	}
}

// TestGroupSend 验证多用户群发：一个文件同时推给多个用户。
func TestGroupSend(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dst, 0o755)
	srcFile := filepath.Join(dst, "share.bin")
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	conn1, token1 := connectUser(t, addr, 0, "")
	defer conn1.Close()
	conn2, token2 := connectUser(t, addr, 0, "")
	defer conn2.Close()
	waitUser(t, sh, token1)
	waitUser(t, sh, token2)

	if !sh.execTp([]string{"tp", "share.bin", "1,2"}) {
		t.Fatal("群发返回 false")
	}

	br1 := bufio.NewReaderSize(conn1, 256*1024)
	br2 := bufio.NewReaderSize(conn2, 256*1024)
	m1 := readCtrl(t, br1)
	m2 := readCtrl(t, br2)
	if m1.Type != "pull" || m2.Type != "pull" {
		t.Fatalf("应收到 pull 消息: %+v %+v", m1, m2)
	}

	dir1 := filepath.Join(root, "c1")
	dir2 := filepath.Join(root, "c2")
	os.MkdirAll(dir1, 0o755)
	os.MkdirAll(dir2, 0o755)
	ctx, cctx := context.WithCancel(context.Background())
	defer cctx()
	rcv1 := newReceiver(dir1, "c1", false)
	rcv2 := newReceiver(dir2, "c2", false)
	for _, r := range []*receiver{rcv1, rcv2} {
		go r.progressLoop(ctx)
		go r.watchdog(ctx)
		go r.settleGroups(ctx)
		defer r.closeAll()
	}

	if err := pullFile(ctx, dialAddr(addr), token1, rcv1, m1.Name, m1.Size, m1.Auth, 4, 2, false); err != nil {
		t.Fatalf("用户1 拉取失败: %v", err)
	}
	if err := pullFile(ctx, dialAddr(addr), token2, rcv2, m2.Name, m2.Size, m2.Auth, 4, 2, false); err != nil {
		t.Fatalf("用户2 拉取失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dir1, "share.bin")); got != sha256file(t, srcFile) {
		t.Fatal("用户1 文件内容不一致")
	}
	if got := sha256file(t, filepath.Join(dir2, "share.bin")); got != sha256file(t, srcFile) {
		t.Fatal("用户2 文件内容不一致")
	}
}

// TestExecTpJobs 验证 tp 指令的 -j 参数与多用户解析。
func TestExecTpJobs(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	os.MkdirAll(dst, 0o755)
	srcFile := filepath.Join(dst, "f.bin")
	if err := os.WriteFile(srcFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn1, token1 := connectUser(t, addr, 0, "")
	defer conn1.Close()
	conn2, token2 := connectUser(t, addr, 0, "")
	defer conn2.Close()
	waitUser(t, sh, token1)
	waitUser(t, sh, token2)

	if !sh.execTp([]string{"tp", "f.bin", "1", "-j", "8"}) {
		t.Fatal("tp -j 返回 false")
	}
	br1 := bufio.NewReaderSize(conn1, 256*1024)
	m := readCtrl(t, br1)
	if m.Type != "pull" || m.Jobs != 8 || m.PushID == "" {
		t.Fatalf("pull 消息应带 Jobs=8 与 PushID: %+v", m)
	}

	if !sh.execTp([]string{"tp", "f.bin", "1,2", "-j", "3"}) {
		t.Fatal("群发 -j 返回 false")
	}
	m1 := readCtrl(t, br1)
	m2 := readCtrl(t, bufio.NewReaderSize(conn2, 256*1024))
	if m1.Jobs != 3 || m2.Jobs != 3 {
		t.Fatalf("群发 Jobs 应为 3: %+v %+v", m1, m2)
	}
	if m1.PushID == m2.PushID {
		t.Fatal("不同用户的推送批次 ID 应不同")
	}
}

// TestGetTransferConcurrent 并发分块连接同时创建同一传输时，必须只生成一个目标文件，
// 且所有分块拿到同一个 transfer（防止各自 uniqueOpen 生成不同文件导致内容错位）。
func TestGetTransferConcurrent(t *testing.T) {
	dir := t.TempDir()
	rcv := newReceiver(dir, "测试", false)
	const n = 16
	trs := make([]*transfer, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr, err := rcv.getTransfer(chunkHeader{V: protoVersion, ID: "same-id", User: "tok", Name: "x.bin", Size: 100, Chunks: n})
			if err != nil {
				t.Errorf("getTransfer: %v", err)
				return
			}
			trs[i] = tr
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if trs[i] != trs[0] {
			t.Fatal("并发分块拿到了不同的 transfer")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "x.bin" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("应只生成 x.bin，实际: %v", names)
	}
	rcv.closeAll() // 关闭句柄，便于 TempDir 清理
}
