package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// clientShell 客户端状态：控制连接、入站监听、接收引擎。
type clientShell struct {
	ctx      context.Context
	server   string
	proxy    string
	conn     net.Conn
	token    string
	ln       net.Listener // 入站监听，接收服务端推送
	inPort   int
	rcv      *receiver
	threads  int
	retries  int
	jobs     int
	verbose  bool
	lastPing int64
}

// runClientInteractive 连接服务端并进入交互会话。
func runClientInteractive(ctx context.Context, server, proxy string, threads, retries, jobs int, verbose bool) error {
	conn, err := dialTarget(proxy, server)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", server, err)
	}
	token := randomID()
	sh := &clientShell{
		ctx:     ctx,
		server:  server,
		proxy:   proxy,
		conn:    conn,
		token:   token,
		threads: threads,
		retries: retries,
		jobs:    jobs,
		verbose: verbose,
	}
	sh.rcv = newReceiver(".", "客户端", verbose)
	if ln, err := net.Listen("tcp", ":0"); err == nil {
		sh.ln = ln
		sh.inPort = ln.Addr().(*net.TCPAddr).Port
		go sh.acceptChunks(ctx)
	}
	if err := writeJSONLine(conn, ctrlMsg{Type: "hello", Token: token, Port: sh.inPort}); err != nil {
		return fmt.Errorf("发送会话信息失败: %w", err)
	}
	go sh.rcv.progressLoop(ctx)
	go sh.rcv.watchdog(ctx)
	go sh.rcv.settleGroups(ctx)

	printLine("已连接 %s", server)
	printLine("输入 help 查看指令；stop 或 Ctrl+C 断开")
	if sh.inPort == 0 {
		printLine("提示: 本机无法监听端口，服务端将不能推送文件给你")
	}

	ctrlCh := make(chan ctrlMsg, 16)
	br := bufio.NewReaderSize(conn, 256*1024)
	go func() {
		for {
			line, err := readLineLimited(br, maxHeaderLen)
			if err != nil {
				close(ctrlCh)
				return
			}
			var m ctrlMsg
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			select {
			case ctrlCh <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	cmdCh := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			select {
			case cmdCh <- strings.TrimSpace(sc.Text()):
			case <-ctx.Done():
				return
			}
		}
		close(cmdCh)
	}()

	for {
		prompt()
		select {
		case <-ctx.Done():
			sh.close()
			printLine("已断开连接")
			return nil
		case m, ok := <-ctrlCh:
			if !ok {
				sh.close()
				printLine("服务端连接已断开")
				return nil
			}
			if !sh.handleCtrl(m) {
				sh.close()
				return nil
			}
		case cmd, ok := <-cmdCh:
			if !ok {
				sh.close()
				return nil
			}
			clearPromptFlag()
			if cmd == "" {
				continue
			}
			if !sh.execCmd(cmd) {
				writeJSONLine(conn, ctrlMsg{Type: "bye"})
				sh.close()
				printLine("已断开连接")
				return nil
			}
		}
	}
}

func (sh *clientShell) close() {
	if sh.conn != nil {
		sh.conn.Close()
	}
	if sh.ln != nil {
		sh.ln.Close()
	}
	sh.rcv.closeAll()
}

// handleCtrl 处理来自服务端的控制消息。返回 false 表示会话结束。
func (sh *clientShell) handleCtrl(m ctrlMsg) bool {
	switch m.Type {
	case "kick":
		printLine("被管理员踢出: %s", m.Msg)
		return false
	case "ping":
		writeJSONLine(sh.conn, ctrlMsg{Type: "pong", Ts: m.Ts})
		return true
	case "pong":
		rtt := time.Since(time.Unix(0, m.Ts))
		printLine("延迟: %s", rtt.Round(time.Microsecond))
		return true
	case "ls_req":
		writeJSONLine(sh.conn, ctrlMsg{Type: "ls_resp", Entries: listDir(".")})
		return true
	case "ls_resp":
		printLine("服务端目录:")
		for _, e := range m.Entries {
			printLine("  %s", e)
		}
		return true
	case "send":
		// 服务端 tp -me: 把本地文件发给服务端
		go sh.sendLocal(sh.ctx, m.Name)
		return true
	default:
		printLine("未知控制消息: %s", m.Type)
		return true
	}
}

func (sh *clientShell) execCmd(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	switch fields[0] {
	case "help":
		printLine("客户端指令: tp 文件/文件夹 | tp -me 服务端文件 | ls | ping | stop")
		return true
	case "stop":
		return false
	case "ping":
		sh.lastPing = time.Now().UnixNano()
		writeJSONLine(sh.conn, ctrlMsg{Type: "ping", Ts: sh.lastPing})
		return true
	case "ls":
		writeJSONLine(sh.conn, ctrlMsg{Type: "ls_req"})
		return true
	case "tp":
		if len(fields) >= 2 && fields[1] == "-me" {
			if len(fields) < 3 {
				printLine("用法: tp -me 服务端目录里的文件")
				return true
			}
			if sh.inPort == 0 {
				printLine("本机无法接收推送，请检查监听端口")
				return true
			}
			writeJSONLine(sh.conn, ctrlMsg{Type: "send", Name: fields[2]})
			return true
		}
		if len(fields) < 2 {
			printLine("用法: tp 文件或文件夹")
			return true
		}
		go sh.sendLocal(sh.ctx, fields[1])
		return true
	default:
		printLine("未知指令: %s（输入 help 查看）", fields[0])
		return true
	}
}

// sendLocal 把本地文件/目录发送到服务端（客户端作为发送端）。
func (sh *clientShell) sendLocal(ctx context.Context, path string) {
	items, err := collectFiles([]string{path})
	if err != nil {
		printLine("发送 %s 失败: %v", path, err)
		return
	}
	dial := func() (net.Conn, error) {
		return dialTarget(sh.proxy, sh.server)
	}
	if err := sendItems(ctx, dial, sh.token, items, sh.threads, sh.retries, sh.jobs, sh.verbose); err != nil {
		printLine("发送 %s 失败: %v", path, err)
		return
	}
	printLine("已发送 %s (%d 个文件)", path, len(items))
}

// acceptChunks 接受服务端推来的分块连接（客户端作为接收端）。
func (sh *clientShell) acceptChunks(ctx context.Context) {
	for {
		conn, err := sh.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReaderSize(c, 256*1024)
			h, err := readHeader(br)
			if err != nil {
				return
			}
			if h.User != sh.token {
				return
			}
			sh.rcv.handleChunkConn(c, br, h)
		}(conn)
	}
}
