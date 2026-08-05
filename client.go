package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
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
	pullSem  chan struct{} // 拉取并发上限（=jobs），防止大文件夹推送打爆连接数
	pushMu   sync.Mutex
	pushSems map[string]chan struct{} // 按推送批次（pushID）隔离的拉取并发信号量（tp -j）
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
		pullSem: make(chan struct{}, jobs),
	}
	sh.rcv = newReceiver(".", "客户端", verbose)
	if ln, err := net.Listen("tcp", ":0"); err == nil {
		sh.ln = ln
		sh.inPort = ln.Addr().(*net.TCPAddr).Port
		go sh.acceptChunks(ctx)
	}
	if err := writeJSONLine(conn, ctrlMsg{Type: "hello", V: pullProtoVer, Token: token, Port: sh.inPort}); err != nil {
		return fmt.Errorf("发送会话信息失败: %w", err)
	}
	go sh.rcv.progressLoop(ctx)
	go sh.rcv.watchdog(ctx)
	go sh.rcv.settleGroups(ctx)

	printOK("已连接 %s", server)
	printDim("输入 help 查看指令；stop 或 Ctrl+C 断开")

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

	cmdCh := readLines(ctx)

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
		printErr("被管理员踢出: %s", m.Msg)
		return false
	case "ping":
		writeJSONLine(sh.conn, ctrlMsg{Type: "pong", Ts: m.Ts})
		return true
	case "pong":
		rtt := time.Since(time.Unix(0, m.Ts))
		printOK("来自服务端 (%s) 的回复: 时间=%s", hostOf(sh.server), fmtRTT(rtt))
		return true
	case "ls_req":
		writeJSONLine(sh.conn, ctrlMsg{Type: "ls_resp", Entries: listPathEntries(".", m.Name)})
		return true
	case "ls_resp":
		printList("服务端目录:", m.Entries)
		return true
	case "send":
		// 服务端 tp -me: 把本地文件发给服务端
		go sh.sendLocal(sh.ctx, m.Name, sh.jobs)
		return true
	case "pull":
		// 服务端 tp 文件 用户id: 通知本机主动拉取（数据连接由客户端发起，NAT 下也能通）
		// 用信号量限制并发拉取数，避免大文件夹推送时同时建立上万条连接；
		// 服务端 tp ... -j 指定并发时按推送批次隔离，否则用全局 -j 上限
		go func(m ctrlMsg) {
			var sem chan struct{}
			if m.PushID != "" {
				jobs := m.Jobs
				if jobs < 1 {
					jobs = sh.jobs
				}
				sh.pushMu.Lock()
				if sh.pushSems == nil {
					sh.pushSems = make(map[string]chan struct{})
				}
				var ok bool
				sem, ok = sh.pushSems[m.PushID]
				if !ok {
					sem = make(chan struct{}, jobs)
					sh.pushSems[m.PushID] = sem
				}
				sh.pushMu.Unlock()
			} else {
				sem = sh.pullSem
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-sh.ctx.Done():
				return
			}
			sh.pullFromServer(m.Name, m.Size, m.Auth)
		}(m)
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
		printDim("客户端指令: tp 文件/文件夹 | tp -me 服务端文件 | ls [路径] | lst [路径] | ping | stop")
		return true
	case "stop":
		return false
	case "ping":
		sh.lastPing = time.Now().UnixNano()
		writeJSONLine(sh.conn, ctrlMsg{Type: "ping", Ts: sh.lastPing})
		return true
	case "ls":
		// ls 列出本地当前目录
		path := ""
		if len(fields) > 1 {
			path = fields[1]
		}
		printList("", listPathEntries(".", path))
		return true
	case "lst":
		// lst 列出服务端当前目录
		path := ""
		if len(fields) > 1 {
			path = fields[1]
		}
		writeJSONLine(sh.conn, ctrlMsg{Type: "ls_req", Name: path})
		return true
	case "tp":
		me := false
		rest := fields[1:]
		if len(rest) > 0 && rest[0] == "-me" {
			me = true
			rest = rest[1:]
		}
		// tp ... -j 个数：本次上传的并发文件数
		jobs := sh.jobs
		var paths []string
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-j" {
				if i+1 >= len(rest) {
					printDim("用法: tp 文件或文件夹 [-j 并发数]")
					return true
				}
				n, err := strconv.Atoi(rest[i+1])
				if err != nil || n < 1 {
					printDim("无效的 -j 参数: %s", rest[i+1])
					return true
				}
				jobs = n
				i++
				continue
			}
			paths = append(paths, rest[i])
		}
		if len(paths) == 0 {
			printDim("用法: tp 文件或文件夹 [-j 并发数]")
			return true
		}
		if me {
			writeJSONLine(sh.conn, ctrlMsg{Type: "send", Name: paths[0]})
			return true
		}
		go sh.sendLocal(sh.ctx, paths[0], jobs)
		return true
	default:
		printErr("未知指令: %s（输入 help 查看）", fields[0])
		return true
	}
}

// sendLocal 把本地文件/目录发送到服务端（客户端作为发送端）。
func (sh *clientShell) sendLocal(ctx context.Context, path string, jobs int) {
	items, err := collectFiles([]string{path})
	if err != nil {
		printErr("发送 %s 失败: %v", path, err)
		return
	}
	if jobs < 1 {
		jobs = sh.jobs
	}
	dial := func() (net.Conn, error) {
		return dialTarget(sh.proxy, sh.server)
	}
	if err := sendItems(ctx, dial, sh.token, items, sh.threads, sh.retries, jobs, sh.verbose); err != nil {
		printErr("发送 %s 失败: %v", path, err)
		return
	}
	printOK("已发送 %s (%d 个文件)", path, len(items))
}

// pullFromServer 响应服务端推送：以客户端为发起方，向服务端建立分块连接拉取文件。
func (sh *clientShell) pullFromServer(name string, size int64, auth string) {
	if size < 0 {
		printErr("拉取 %s 失败: 文件大小无效", name)
		return
	}
	dial := func() (net.Conn, error) {
		return dialTarget(sh.proxy, sh.server)
	}
	ctx, cancel := context.WithTimeout(sh.ctx, 30*time.Minute)
	defer cancel()
	if err := pullFile(ctx, dial, sh.token, sh.rcv, name, size, auth, sh.threads, sh.retries, sh.verbose); err != nil {
		writeJSONLine(sh.conn, ctrlMsg{Type: "pull_done", Auth: auth, Name: name, Msg: err.Error()})
		printErr("拉取 %s 失败: %v", name, err)
		return
	}
	writeJSONLine(sh.conn, ctrlMsg{Type: "pull_done", Auth: auth, Name: name})
	printOK("已从服务端接收 %s", name)
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
			line, err := readLineLimited(br, maxHeaderLen)
			if err != nil {
				return
			}
			// 断点续传查询：发送端询问本机已存在哪些分块
			var rq resumeQuery
			if json.Unmarshal(line, &rq) == nil && rq.User == sh.token && rq.Name != "" && rq.Chunks > 0 && rq.V == protoVersion {
				replyResumeQuery(c, sh.rcv, rq.Name, rq.Size, rq.Chunks)
				return
			}
			var h chunkHeader
			if err := json.Unmarshal(line, &h); err != nil || h.ID == "" {
				return
			}
			if h.User != sh.token {
				return
			}
			if h.V != protoVersion || h.Chunk < 0 || h.Chunks <= 0 || h.Chunk >= h.Chunks ||
				h.Start < 0 || h.Len < 0 || h.Start+h.Len > h.Size {
				return
			}
			sh.rcv.handleChunkConn(c, br, h)
		}(conn)
	}
}
