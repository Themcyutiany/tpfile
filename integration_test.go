package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

func startTestServer(t *testing.T, port int, dst string) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = runServer(ctx, port, dst, false)
	}()
	time.Sleep(150 * time.Millisecond)
	return cancel
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
	cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := runClient(context.Background(), addr, []string{srcFile}, "", 4, 2, false); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "big.bin")); got != sha256file(t, srcFile) {
		t.Fatal("文件内容不一致")
	}
}

func TestTransferDir(t *testing.T) {
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
	cancel := startTestServer(t, port, dst)
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := runClient(context.Background(), addr, []string{src}, "", 4, 2, false); err != nil {
		t.Fatalf("发送目录失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".minecraft")); err != nil {
		t.Fatalf("顶层目录 .minecraft 未在接收端创建: %v", err)
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

func TestTransferIPv6(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")

	payload := make([]byte, 1<<20)
	rand.Read(payload)
	srcFile := filepath.Join(root, "v6.bin")
	os.WriteFile(srcFile, payload, 0o644)

	port := freePort(t)
	cancel := startTestServer(t, port, dst)
	defer cancel()

	// 确认本机 IPv6 回环可用
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("[::1]:%d", port), 2*time.Second)
	if err != nil {
		t.Skip("本机不支持 IPv6 回环")
	}
	conn.Close()

	if err := runClient(context.Background(), fmt.Sprintf("[::1]:%d", port), []string{srcFile}, "", 2, 2, false); err != nil {
		t.Fatalf("IPv6 传输失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "v6.bin")); got != sha256file(t, srcFile) {
		t.Fatal("IPv6 文件内容不一致")
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
	if greet[1] > 0 { // 读取剩余的方法列表，避免遗留字节污染后续读取
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
	// 双向转发；等两端都关闭后再关闭连接，避免提前 Close 触发 RST
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
	rand.Read(payload)
	srcFile := filepath.Join(root, "proxy.bin")
	os.WriteFile(srcFile, payload, 0o644)

	port := freePort(t)
	cancel := startTestServer(t, port, dst)
	defer cancel()

	proxyAddr, cancelProxy := startSocks5Proxy(t)
	defer cancelProxy()

	if err := runClient(context.Background(), fmt.Sprintf("127.0.0.1:%d", port), []string{srcFile}, proxyAddr, 3, 2, false); err != nil {
		t.Fatalf("代理传输失败: %v", err)
	}
	if got := sha256file(t, filepath.Join(dst, "proxy.bin")); got != sha256file(t, srcFile) {
		t.Fatal("代理传输文件内容不一致")
	}
}

func TestEmptyFile(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	srcFile := filepath.Join(root, "empty.txt")
	os.WriteFile(srcFile, nil, 0o644)

	port := freePort(t)
	cancel := startTestServer(t, port, dst)
	defer cancel()

	if err := runClient(context.Background(), fmt.Sprintf("127.0.0.1:%d", port), []string{srcFile}, "", 4, 2, false); err != nil {
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
