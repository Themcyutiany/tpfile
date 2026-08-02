package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCompletePath 验证 Tab 补全逻辑。
func TestCompletePath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"abc.txt", "abd.txt", "subdir", "other.log"} {
		p := filepath.Join(dir, name)
		if name == "subdir" {
			os.MkdirAll(p, 0o755)
		} else {
			os.WriteFile(p, []byte("x"), 0o644)
		}
	}
	old, _ := os.Getwd()
	defer os.Chdir(old)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// 1) 唯一匹配：直接补全
	buf, pos, cands := completePath([]rune("tp abc"), 6)
	if got := string(buf); got != "tp abc.txt" {
		t.Fatalf("唯一匹配补全: %q", got)
	}
	if pos != len([]rune("tp abc.txt")) {
		t.Fatalf("光标位置: %d", pos)
	}
	if len(cands) != 1 || cands[0] != "abc.txt" {
		t.Fatalf("候选: %v", cands)
	}

	// 2) 目录唯一匹配：补全并带 /
	buf, _, _ = completePath([]rune("tp sub"), 6)
	if got := string(buf); got != "tp subdir/" {
		t.Fatalf("目录补全: %q", got)
	}

	// 3) 多个匹配且公共前缀可扩展
	buf, _, cands = completePath([]rune("tp a"), 4)
	if got := string(buf); got != "tp ab" {
		t.Fatalf("公共前缀扩展: %q", got)
	}
	if len(cands) != 2 {
		t.Fatalf("候选: %v", cands)
	}

	// 4) 公共前缀不再增长：返回原缓冲 + 候选
	buf, _, cands = completePath([]rune("tp ab"), 5)
	if string(buf) != "tp ab" || len(cands) != 2 {
		t.Fatalf("无扩展: buf=%q cands=%v", string(buf), cands)
	}

	// 5) 无匹配
	buf, _, cands = completePath([]rune("tp zz"), 5)
	if string(buf) != "tp zz" || cands != nil {
		t.Fatalf("无匹配: buf=%q cands=%v", string(buf), cands)
	}

	// 6) 子目录补全
	buf, _, _ = completePath([]rune("tp subd"), 7)
	if got := string(buf); got != "tp subdir/" {
		t.Fatalf("子目录补全: %q", got)
	}
}

// TestListPathEntries 验证带子路径的目录列出与路径安全。
func TestListPathEntries(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "top.txt"), []byte("x"), 0o644)

	got := listPathEntries(dir, "")
	if len(got) != 2 {
		t.Fatalf("根目录条目: %v", got)
	}
	got = listPathEntries(dir, "sub")
	if len(got) != 1 || got[0] != "a.txt" {
		t.Fatalf("子目录条目: %v", got)
	}
	abs := "/etc"
	if runtime.GOOS == "windows" {
		abs = `C:\Windows`
	}
	got = listPathEntries(dir, abs)
	if len(got) != 1 || got[0] != "<路径无效>" {
		t.Fatalf("绝对路径应被拒绝: %v", got)
	}
}

// TestServerLsLocalAndLst 验证服务端 ls（本地）与 lst（远程）指令。
func TestServerLsLocalAndLst(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	os.MkdirAll(filepath.Join(dst, "sub"), 0o755)
	os.WriteFile(filepath.Join(dst, "hello.txt"), []byte("hi"), 0o644)

	port := freePort(t)
	sh, cancel := startTestServer(t, port, dst)
	defer cancel()

	// ls 列出本地保存目录（不崩溃即可，输出走 stdout）
	if !sh.execCmd("ls") {
		t.Fatal("ls 返回 false")
	}
	if !sh.execCmd("ls sub") {
		t.Fatal("ls sub 返回 false")
	}

	// lst 用户id → 客户端收到 ls_req（带路径）
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, token := connectUser(t, addr, 0, "")
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)

	u := waitUser(t, sh, token)
	if !sh.execCmd(fmt.Sprintf("lst %d", u.id)) {
		t.Fatal("lst 返回 false")
	}
	m := readCtrl(t, br)
	if m.Type != "ls_req" || m.Name != "" {
		t.Fatalf("意外消息: %+v", m)
	}

	if !sh.execCmd(fmt.Sprintf("lst %d sub", u.id)) {
		t.Fatal("lst 带路径返回 false")
	}
	m = readCtrl(t, br)
	if m.Type != "ls_req" || m.Name != "sub" {
		t.Fatalf("意外消息: %+v", m)
	}
}

// TestEditorControlKeys 验证行编辑器控制键映射。
func TestEditorControlKeys(t *testing.T) {
	// Ctrl+C → 输出 stop
	outMu.Lock()
	editBuf = editBuf[:0]
	editPos = 0
	outMu.Unlock()
	if done, line := handleCtrlKey(0x03); !done || line != "stop" {
		t.Fatalf("Ctrl+C: done=%v line=%q", done, line)
	}

	// Enter → 结束输入并返回缓冲内容
	outMu.Lock()
	editBuf = editBuf[:0]
	editPos = 0
	outMu.Unlock()
	if done, line := handleCtrlKey(0x0d); !done || line != "" {
		t.Fatalf("Enter: done=%v line=%q", done, line)
	}

	// 空缓冲上按退格不应崩溃也不应结束输入
	outMu.Lock()
	editBuf = editBuf[:0]
	editPos = 0
	outMu.Unlock()
	if done, _ := handleCtrlKey(0x7f); done {
		t.Fatal("Backspace 不应结束输入")
	}

	// Ctrl+D 空缓冲 → EOF
	outMu.Lock()
	editBuf = editBuf[:0]
	editPos = 0
	outMu.Unlock()
	if done, line := handleCtrlKey(0x04); !done || line != "" {
		t.Fatalf("Ctrl+D: done=%v line=%q", done, line)
	}
}

// TestInteractiveServerNonTTY 用管道模拟非终端输入输出，验证服务端交互循环。
func TestInteractiveServerNonTTY(t *testing.T) {
	oldIn, oldOut := os.Stdin, os.Stdout
	rIn, wIn, _ := os.Pipe()
	rOut, wOut, _ := os.Pipe()
	os.Stdin, os.Stdout = rIn, wOut
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runServerInteractive(ctx, port, t.TempDir(), 4, 2, 4, false) }()

	go func() {
		wIn.WriteString("list\n")
		time.Sleep(150 * time.Millisecond)
		wIn.WriteString("stop\n")
		wIn.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("服务端交互循环错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("服务端交互循环超时")
	}
	wOut.Close()
	out, _ := io.ReadAll(rOut)
	if !strings.Contains(string(out), "服务端已停止") {
		t.Fatalf("未看到停止输出: %q", string(out))
	}
}
