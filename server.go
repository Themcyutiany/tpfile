package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// user 是一个已连接的客户端会话。
type user struct {
	id     int
	token  string
	conn   net.Conn
	wmu    sync.Mutex // 保护 conn 写入
	ip     string
	inPort int // 客户端入站传输端口（0 表示不支持接收推送）
}

// serverShell 服务端状态：监听、用户管理、接收引擎。
type serverShell struct {
	ctx     context.Context
	dir     string
	threads int
	retries int
	jobs    int
	verbose bool
	ln      net.Listener
	rcv     *receiver
	stopped atomic.Bool
	usersMu sync.Mutex
	users   map[int]*user
	nextID  int
	stopOne sync.Once
}

// startServerShell 创建服务端并开始监听（不含交互指令循环）。
func startServerShell(ctx context.Context, port int, dir string, threads, retries, jobs int, verbose bool) (*serverShell, error) {
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建保存目录失败: %w", err)
	}
	addr := net.JoinHostPort("::", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	mode := "IPv4+IPv6"
	if err != nil {
		// 部分系统未开启双栈时退回 IPv4
		ln, err = net.Listen("tcp", ":"+strconv.Itoa(port))
		mode = "IPv4"
		if err != nil {
			return nil, fmt.Errorf("监听端口 %d 失败: %w", port, err)
		}
	}
	sh := &serverShell{
		ctx:     ctx,
		dir:     dir,
		threads: threads,
		retries: retries,
		jobs:    jobs,
		verbose: verbose,
		ln:      ln,
		users:   make(map[int]*user),
		nextID:  1,
	}
	sh.rcv = newReceiver(dir, "服务端", verbose)
	go sh.rcv.progressLoop(ctx)
	go sh.rcv.watchdog(ctx)
	go sh.rcv.settleGroups(ctx)
	go sh.acceptLoop(ctx)
	printLine("tpfile 服务端已启动，监听 %s (%s)", ln.Addr(), mode)
	printLine("接收的文件将保存到: %s", dir)
	printLine("提示: 以 . 开头的目录(如 .minecraft)在 Linux 下默认隐藏, 用 ls -a 查看")
	printLine("输入 help 查看服务端指令；stop 或 Ctrl+C 停止")
	return sh, nil
}

// runServerInteractive 启动服务端并进入交互指令循环。
func runServerInteractive(ctx context.Context, port int, dir string, threads, retries, jobs int, verbose bool) error {
	sh, err := startServerShell(ctx, port, dir, threads, retries, jobs, verbose)
	if err != nil {
		return err
	}
	defer sh.shutdown()
	return sh.cmdLoop(ctx)
}

func (sh *serverShell) shutdown() {
	sh.stopOne.Do(func() {
		sh.stopped.Store(true)
		sh.ln.Close()
		sh.usersMu.Lock()
		for _, u := range sh.users {
			u.conn.Close()
		}
		sh.usersMu.Unlock()
		sh.rcv.closeAll()
	})
}

// acceptLoop 接受新连接：分块连接直接进入接收引擎，控制连接注册为用户。
func (sh *serverShell) acceptLoop(ctx context.Context) {
	for {
		conn, err := sh.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || sh.stopped.Load() {
				return
			}
			printLine("接受连接失败: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go sh.dispatchConn(ctx, conn)
	}
}

func (sh *serverShell) dispatchConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 256*1024)
	line, err := readLineLimited(br, maxHeaderLen)
	if err != nil {
		return
	}
	// 分块连接：首部含 id/user
	var h chunkHeader
	if err := json.Unmarshal(line, &h); err == nil && h.ID != "" {
		if !sh.validToken(h.User) {
			return
		}
		sh.rcv.handleChunkConn(conn, br, h)
		return
	}
	// 控制连接：第一条必须是 hello
	var m ctrlMsg
	if err := json.Unmarshal(line, &m); err != nil || m.Type != "hello" {
		return
	}
	u := sh.registerUser(conn, m)
	if u == nil {
		return
	}
	printLine("[用户 %d] 已连接 (%s)%s", u.id, u.ip, inPortSuffix(u))
	defer func() {
		sh.unregisterUser(u)
		printLine("[用户 %d] 已断开", u.id)
	}()
	for {
		line, err := readLineLimited(br, maxHeaderLen)
		if err != nil {
			return
		}
		var m ctrlMsg
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		if !sh.handleCtrl(u, m) {
			return
		}
	}
}

func (sh *serverShell) registerUser(conn net.Conn, m ctrlMsg) *user {
	sh.usersMu.Lock()
	defer sh.usersMu.Unlock()
	token := m.Token
	if token == "" {
		token = randomID()
	}
	for sh.tokenExistsLocked(token) {
		token = randomID()
	}
	u := &user{id: sh.nextID, token: token, conn: conn, ip: hostOf(conn.RemoteAddr().String()), inPort: m.Port}
	sh.nextID++
	sh.users[u.id] = u
	return u
}

func (sh *serverShell) unregisterUser(u *user) {
	sh.usersMu.Lock()
	delete(sh.users, u.id)
	sh.usersMu.Unlock()
}

func (sh *serverShell) tokenExistsLocked(token string) bool {
	for _, u := range sh.users {
		if u.token == token {
			return true
		}
	}
	return false
}

func (sh *serverShell) validToken(token string) bool {
	sh.usersMu.Lock()
	defer sh.usersMu.Unlock()
	return sh.tokenExistsLocked(token)
}

func (sh *serverShell) userByToken(token string) *user {
	sh.usersMu.Lock()
	defer sh.usersMu.Unlock()
	for _, u := range sh.users {
		if u.token == token {
			return u
		}
	}
	return nil
}

func (sh *serverShell) userByID(id int) *user {
	sh.usersMu.Lock()
	defer sh.usersMu.Unlock()
	return sh.users[id]
}

func (sh *serverShell) sendCtrl(u *user, m ctrlMsg) {
	u.wmu.Lock()
	defer u.wmu.Unlock()
	writeJSONLine(u.conn, m)
}

// handleCtrl 处理来自客户端的控制消息。
func (sh *serverShell) handleCtrl(u *user, m ctrlMsg) bool {
	switch m.Type {
	case "ping":
		sh.sendCtrl(u, ctrlMsg{Type: "pong", Ts: m.Ts})
	case "pong":
		rtt := time.Since(time.Unix(0, m.Ts))
		printLine("[用户 %d] 延迟: %s", u.id, rtt.Round(time.Microsecond))
	case "ls_req":
		sh.sendCtrl(u, ctrlMsg{Type: "ls_resp", Entries: listDir(sh.rcv.dir)})
	case "ls_resp":
		printLine("[用户 %d] 的目录:", u.id)
		for _, e := range m.Entries {
			printLine("  %s", e)
		}
	case "send":
		// 客户端 tp -me: 服务端把文件推送给客户端
		go sh.pushToUser(u, m.Name)
	case "bye":
		return false
	default:
		printLine("[用户 %d] 未知控制消息: %s", u.id, m.Type)
	}
	return true
}

// cmdLoop 读取并执行服务端指令，直到 stop 或 ctx 取消。
func (sh *serverShell) cmdLoop(ctx context.Context) error {
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
			return nil
		case cmd, ok := <-cmdCh:
			if !ok {
				return nil
			}
			clearPromptFlag()
			if cmd == "" {
				continue
			}
			if !sh.execCmd(cmd) {
				printLine("服务端已停止")
				return nil
			}
		}
	}
}

func (sh *serverShell) execCmd(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	switch fields[0] {
	case "help":
		printLine("服务端指令: ls | ls 用户id | ping 用户id | kick 用户id | tp 文件 用户id | tp -me 文件 用户id | stop")
		return true
	case "stop":
		return false
	case "ls":
		if len(fields) == 1 {
			sh.listUsers()
			return true
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			printLine("用法: ls 用户id")
			return true
		}
		u := sh.userByID(id)
		if u == nil {
			printLine("用户 %d 不存在", id)
			return true
		}
		sh.sendCtrl(u, ctrlMsg{Type: "ls_req"})
		return true
	case "ping":
		if len(fields) < 2 {
			printLine("用法: ping 用户id")
			return true
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			printLine("用户id 无效")
			return true
		}
		u := sh.userByID(id)
		if u == nil {
			printLine("用户 %d 不存在", id)
			return true
		}
		sh.sendCtrl(u, ctrlMsg{Type: "ping", Ts: time.Now().UnixNano()})
		return true
	case "kick":
		if len(fields) < 2 {
			printLine("用法: kick 用户id")
			return true
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			printLine("用户id 无效")
			return true
		}
		u := sh.userByID(id)
		if u == nil {
			printLine("用户 %d 不存在", id)
			return true
		}
		sh.sendCtrl(u, ctrlMsg{Type: "kick", Msg: "管理员将你踢出"})
		u.conn.Close()
		sh.unregisterUser(u)
		printLine("已踢出用户 %d", id)
		return true
	case "tp":
		return sh.execTp(fields)
	default:
		printLine("未知指令: %s（输入 help 查看）", fields[0])
		return true
	}
}

// execTp 处理服务端 tp 指令: tp 文件 用户id / tp -me 文件 用户id。
func (sh *serverShell) execTp(fields []string) bool {
	me := false
	rest := fields[1:]
	if len(rest) > 0 && rest[0] == "-me" {
		me = true
		rest = rest[1:]
	}
	if len(rest) < 2 {
		printLine("用法: tp 文件 用户id 或 tp -me 文件 用户id")
		return true
	}
	path, idStr := rest[0], rest[1]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		printLine("用户id 无效")
		return true
	}
	u := sh.userByID(id)
	if u == nil {
		printLine("用户 %d 不存在", id)
		return true
	}
	if me {
		// 拉取: 通知客户端把本地文件发到服务端
		sh.sendCtrl(u, ctrlMsg{Type: "send", Name: path})
		return true
	}
	// 推送: 服务端文件发给客户端
	if u.inPort == 0 {
		printLine("用户 %d 未开放接收端口，无法推送", id)
		return true
	}
	go sh.pushToUser(u, path)
	return true
}

// pushToUser 把服务端文件推送给指定用户（服务端作为发送端）。
func (sh *serverShell) pushToUser(u *user, path string) {
	full := path
	if !filepath.IsAbs(path) {
		// 相对路径基于服务端保存目录，与 ls 显示一致
		full = filepath.Join(sh.rcv.dir, path)
	}
	items, err := collectFiles([]string{full})
	if err != nil {
		printLine("发送 %s 给用户 %d 失败: %v", path, u.id, err)
		return
	}
	ip := u.ip
	if ip == "" {
		ip = "127.0.0.1"
	}
	dial := func() (net.Conn, error) {
		return net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(u.inPort)), 5*time.Second)
	}
	if err := sendItems(sh.ctx, dial, u.token, items, sh.threads, sh.retries, sh.jobs, sh.verbose); err != nil {
		printLine("发送 %s 给用户 %d 失败: %v", path, u.id, err)
		return
	}
	printLine("已发送 %s 给用户 %d (%d 个文件)", path, u.id, len(items))
}

func (sh *serverShell) listUsers() {
	sh.usersMu.Lock()
	ids := make([]int, 0, len(sh.users))
	byID := make(map[int]*user, len(sh.users))
	for id, u := range sh.users {
		ids = append(ids, id)
		byID[id] = u
	}
	sh.usersMu.Unlock()
	sort.Ints(ids)
	if len(ids) == 0 {
		printLine("当前没有用户连接")
		return
	}
	for _, id := range ids {
		u := byID[id]
		printLine("用户 %d: %s (令牌 %s, 接收端口 %d)", id, u.ip, shortToken(u.token), u.inPort)
	}
}

func inPortSuffix(u *user) string {
	if u.inPort > 0 {
		return fmt.Sprintf(" (接收端口 %d)", u.inPort)
	}
	return ""
}

func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func shortToken(t string) string {
	if len(t) > 8 {
		return t[:8]
	}
	return t
}
