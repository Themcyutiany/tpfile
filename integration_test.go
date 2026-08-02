package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func sha256file(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startTestServer(t *testing.T, port int, dst string) (*serverShell, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sh, err := startServerShell(ctx, port, dst, 4, 2, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		sh.shutdown()
	})
	return sh, cancel
}

// connectUser 建立一条控制连接并注册会话；token 为空时自动生成。
func connectUser(t *testing.T, addr string, inPort int, token string) (net.Conn, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		token = randomID()
	}
	if err := writeJSONLine(conn, ctrlMsg{Type: "hello", V: pullProtoVer, Token: token, Port: inPort}); err != nil {
		t.Fatal(err)
	}
	return conn, token
}

func waitUser(t *testing.T, sh *serverShell, token string) *user {
	t.Helper()
	for i := 0; i < 100; i++ {
		if u := sh.userByToken(token); u != nil {
			return u
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("用户未注册")
	return nil
}

func waitGone(t *testing.T, sh *serverShell, id int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if sh.userByID(id) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("用户未被移除")
}

func waitFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("文件未出现: %s", path)
}

func readCtrl(t *testing.T, br *bufio.Reader) ctrlMsg {
	t.Helper()
	line, err := readLineLimited(br, maxHeaderLen)
	if err != nil {
		t.Fatal(err)
	}
	var m ctrlMsg
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// runTestClientReceiver 在临时目录上启动一个客户端接收端，返回监听端口。
func runTestClientReceiver(t *testing.T, dir, token string) (int, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rcv := newReceiver(dir, "测试客户端", false)
	go rcv.progressLoop(ctx)
	go rcv.watchdog(ctx)
	go rcv.settleGroups(ctx)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReaderSize(c, 256*1024)
				h, err := readHeader(br)
				if err != nil {
					return
				}
				if h.User != token {
					return
				}
				rcv.handleChunkConn(c, br, h)
			}(c)
		}
	}()
	t.Cleanup(func() {
		cancel()
		ln.Close()
		rcv.closeAll()
	})
	return ln.Addr().(*net.TCPAddr).Port, cancel
}

func dialAddr(addr string) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		return net.DialTimeout("tcp", addr, 5*time.Second)
	}
}

func TestTransferE2E(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	payload := make([]byte, 8<<20)
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

	items, err := collectFiles([]string{srcFile})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendItems(context.Background(), dialAddr(addr), token, items, 4, 2, 4, false); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "big.bin")); got != sha256file(t, srcFile) {
		t.Fatal("文件内容不一致")
	}
}

func TestTransferDirHidden(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, ".minecraft")
	dst := filepath.Join(root, "dst")
	os.MkdirAll(filepath.Join(src, "sub", "deep"), 0o755)

	files := map[string]string{
		"a.txt":          "hello",
		"sub/b.bin":      string(make([]byte, 3<<20)),
		"sub/deep/c.txt": "nested",
	}
	for rel, content := range files {
		p := filepath.Join(src, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	port := freePort(t)
	_, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()

	items, err := collectFiles([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendItems(context.Background(), dialAddr(addr), token, items, 4, 2, 4, false); err != nil {
		t.Fatalf("发送目录失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".minecraft")); err != nil {
		t.Fatalf("顶层目录 .minecraft 未创建: %v", err)
	}
	for rel := range files {
		got, err := os.ReadFile(filepath.Join(dst, ".minecraft", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		if rel == "a.txt" || rel == "sub/deep/c.txt" {
			if string(got) != files[rel] {
				t.Fatalf("%s 内容不一致", rel)
			}
		}
	}
}

func TestServerPush(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	clientDir := filepath.Join(root, "client")
	os.MkdirAll(dst, 0o755)
	os.MkdirAll(clientDir, 0o755)
	// 服务端保存目录里的文件（对应服务端 ls 里看到的文件）
	srcFile := filepath.Join(dst, "push.bin")
	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	waitUser(t, sh, token)

	// 服务端指令: tp 文件 用户id（推送，数据连接由客户端发起）
	if !sh.execTp([]string{"tp", "push.bin", "1"}) {
		t.Fatal("tp 返回 false")
	}
	m := readCtrl(t, br)
	if m.Type != "pull" || m.Name != "push.bin" || m.Size != int64(len(payload)) {
		t.Fatalf("意外消息: %+v", m)
	}

	// 客户端按 pull 消息主动建立分块连接拉取
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	rcv := newReceiver(clientDir, "客户端", false)
	go rcv.progressLoop(ctx)
	go rcv.watchdog(ctx)
	go rcv.settleGroups(ctx)
	if err := pullFile(ctx, dialAddr(addr), token, rcv, m.Name, m.Size, 4, 2, false); err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	rcv.closeAll()

	if got := sha256file(t, filepath.Join(clientDir, "push.bin")); got != sha256file(t, srcFile) {
		t.Fatal("推送文件内容不一致")
	}
}

// TestServerPushOldClient 验证旧版客户端（未上报能力版本）会被提示更新，而不是静默失败。
func TestServerPushOldClient(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	os.MkdirAll(dst, 0o755)
	os.WriteFile(filepath.Join(dst, "old.bin"), []byte("data"), 0o644)

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tok := randomID()
	if err := writeJSONLine(conn, ctrlMsg{Type: "hello", Token: tok, Port: 0}); err != nil {
		t.Fatal(err)
	}
	u := waitUser(t, sh, tok)
	if u == nil || u.ver != 0 {
		t.Fatalf("旧客户端应注册为 ver=0: %+v", u)
	}
	if !sh.execTp([]string{"tp", "old.bin", "1"}) {
		t.Fatal("tp 返回 false")
	}
	// 旧客户端不应收到 pull 消息（版本检查拒绝）
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	line, _ := readLineLimited(bufio.NewReader(conn), maxHeaderLen)
	if len(line) > 0 {
		t.Fatalf("旧客户端不应收到拉取通知: %s", line)
	}
}

func TestPull(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	srcFile := filepath.Join(root, "pull.bin")
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	u := waitUser(t, sh, token)
	// 服务端 tp -me pull.bin 用户id → 向客户端发 send
	sh.sendCtrl(u, ctrlMsg{Type: "send", Name: "pull.bin"})

	m := readCtrl(t, br)
	if m.Type != "send" || m.Name != "pull.bin" {
		t.Fatalf("意外消息: %+v", m)
	}

	items, err := collectFiles([]string{srcFile})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendItems(context.Background(), dialAddr(addr), token, items, 4, 2, 4, false); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "pull.bin")); got != sha256file(t, srcFile) {
		t.Fatal("拉取文件内容不一致")
	}
}

func TestLsAndPing(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	os.MkdirAll(filepath.Join(dst, "sub"), 0o755)
	if err := os.WriteFile(filepath.Join(dst, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	// 客户端 ls → 服务端返回目录
	if err := writeJSONLine(conn, ctrlMsg{Type: "ls_req"}); err != nil {
		t.Fatal(err)
	}
	m := readCtrl(t, br)
	if m.Type != "ls_resp" {
		t.Fatalf("意外消息: %+v", m)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("目录条目数量不对: %v", m.Entries)
	}

	// 客户端 ping → 服务端 pong
	if err := writeJSONLine(conn, ctrlMsg{Type: "ping", Ts: 12345}); err != nil {
		t.Fatal(err)
	}
	m = readCtrl(t, br)
	if m.Type != "pong" || m.Ts != 12345 {
		t.Fatalf("意外消息: %+v", m)
	}

	// 服务端 ping → 客户端 pong
	u := waitUser(t, sh, token)
	sh.sendCtrl(u, ctrlMsg{Type: "ping", Ts: 54321})
	m = readCtrl(t, br)
	if m.Type != "ping" || m.Ts != 54321 {
		t.Fatalf("意外消息: %+v", m)
	}
	if err := writeJSONLine(conn, ctrlMsg{Type: "pong", Ts: m.Ts}); err != nil {
		t.Fatal(err)
	}
}

func TestExecTpMeOrder(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	u := waitUser(t, sh, token)
	// 服务端执行: tp -me 用户id 文件  → 客户端应收到 send 且 Name 为文件
	if !sh.execCmd(fmt.Sprintf("tp -me %d pull.bin", u.id)) {
		t.Fatal("execCmd 返回 false")
	}
	m := readCtrl(t, br)
	if m.Type != "send" || m.Name != "pull.bin" {
		t.Fatalf("意外消息: %+v", m)
	}
}

func TestKick(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	u := waitUser(t, sh, token)
	sh.sendCtrl(u, ctrlMsg{Type: "kick", Msg: "测试踢出"})
	u.conn.Close()

	m := readCtrl(t, br)
	if m.Type != "kick" {
		t.Fatalf("意外消息: %+v", m)
	}
	waitGone(t, sh, u.id)
}

func TestTransferIPv6(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(root, "v6.bin")
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	_, cancel := startTestServer(t, port, dst)
	defer cancel()

	// 确认本机 IPv6 回环可用
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("[::1]:%d", port), 2*time.Second)
	if err != nil {
		t.Skip("本机不支持 IPv6 回环")
	}
	conn.Close()

	addr := fmt.Sprintf("[::1]:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()

	items, err := collectFiles([]string{srcFile})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendItems(context.Background(), dialAddr(addr), token, items, 2, 2, 2, false); err != nil {
		t.Fatalf("IPv6 传输失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "v6.bin")); got != sha256file(t, srcFile) {
		t.Fatal("IPv6 文件内容不一致")
	}
}

func TestEmptyFile(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	srcFile := filepath.Join(root, "empty.txt")
	if err := os.WriteFile(srcFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	_, cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()

	items, err := collectFiles([]string{srcFile})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendItems(context.Background(), dialAddr(addr), token, items, 4, 2, 4, false); err != nil {
		t.Fatalf("空文件传输失败: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dst, "empty.txt"))
	if err != nil {
		t.Fatalf("空文件未正确接收: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("空文件大小应为 0, 实际 %d", fi.Size())
	}
}

// ---- 极简 SOCKS5 测试代理（仅 IPv4 目标） ----

func startSocks5Proxy(t *testing.T) (string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleTestSocks5(c)
		}
	}()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	return ln.Addr().String(), cancel
}

func handleTestSocks5(c net.Conn) {
	defer c.Close()
	greet := make([]byte, 2)
	if _, err := io.ReadFull(c, greet); err != nil {
		return
	}
	if greet[1] > 0 {
		methods := make([]byte, greet[1])
		if _, err := io.ReadFull(c, methods); err != nil {
			return
		}
	}
	c.Write([]byte{5, 0})
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	var addrLen int
	switch head[3] {
	case 1:
		addrLen = 4
	case 4:
		addrLen = 16
	case 3:
		b := make([]byte, 1)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		addrLen = int(b[0])
	default:
		return
	}
	rest := make([]byte, addrLen+2)
	if _, err := io.ReadFull(c, rest); err != nil {
		return
	}
	var host string
	switch head[3] {
	case 1:
		host = net.IP(rest[:4]).String()
	case 4:
		host = net.IP(rest[:16]).String()
	case 3:
		host = string(rest[:addrLen])
	}
	port := int(rest[addrLen])<<8 | int(rest[addrLen+1])
	up, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 5*time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(up, c) }()
	go func() { defer wg.Done(); io.Copy(c, up) }()
	wg.Wait()
}

func TestTransferViaProxy(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(root, "proxy.bin")
	if err := os.WriteFile(srcFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	_, cancel := startTestServer(t, port, dst)
	defer cancel()

	proxyAddr, cancelProxy := startSocks5Proxy(t)
	defer cancelProxy()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := dialTarget(proxyAddr, addr)
	if err != nil {
		t.Fatal(err)
	}
	token := randomID()
	if err := writeJSONLine(conn, ctrlMsg{Type: "hello", Token: token, Port: 0}); err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	items, err := collectFiles([]string{srcFile})
	if err != nil {
		t.Fatal(err)
	}
	dial := func() (net.Conn, error) { return dialTarget(proxyAddr, addr) }
	if err := sendItems(context.Background(), dial, token, items, 3, 2, 3, false); err != nil {
		t.Fatalf("代理传输失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "proxy.bin")); got != sha256file(t, srcFile) {
		t.Fatal("代理传输文件内容不一致")
	}
}
