package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	ver    int // 客户端能力版本（hello 时上报，旧客户端为 0）
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
	printOK("tpfile 服务端已启动，监听 %s (%s)", ln.Addr(), mode)
	printDim("接收的文件将保存到: %s", dir)
	printDim("提示: 以 . 开头的目录(如 .minecraft)也会显示在 ls 列表里")
	printDim("输入 help 查看服务端指令；stop 或 Ctrl+C 停止")
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
			printErr("接受连接失败: %v", err)
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
		if h.Dir == chunkDirOut {
			// 客户端发起的拉取：服务端在连接上直接写出分块数据
			sh.servePullChunk(conn, h)
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
	printInfo("[用户 %d] 已连接 (%s)%s", u.id, u.ip, inPortSuffix(u))
	defer func() {
		sh.unregisterUser(u)
		printInfo("[用户 %d] 已断开", u.id)
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
	u := &user{id: sh.nextID, token: token, conn: conn, ip: hostOf(conn.RemoteAddr().String()), inPort: m.Port, ver: m.V}
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
		printOK("来自用户 %d (%s) 的回复: 时间=%s", u.id, u.ip, fmtRTT(rtt))
	case "ls_req":
		sh.sendCtrl(u, ctrlMsg{Type: "ls_resp", Entries: listPathEntries(sh.rcv.dir, m.Name)})
	case "ls_resp":
		printList(fmt.Sprintf("[用户 %d] 的目录:", u.id), m.Entries)
	case "send":
		// 客户端 tp -me: 服务端把文件推送给客户端（客户端主动拉取）
		go sh.pushToUser(u, m.Name)
	case "pull_done":
		if m.Msg != "" {
			printErr("[用户 %d] 接收 %s 失败: %s", u.id, m.Name, m.Msg)
		} else {
			printOK("[用户 %d] 已接收 %s", u.id, m.Name)
		}
	case "bye":
		return false
	default:
		printErr("[用户 %d] 未知控制消息: %s", u.id, m.Type)
	}
	return true
}

// cmdLoop 读取并执行服务端指令，直到 stop 或 ctx 取消。
func (sh *serverShell) cmdLoop(ctx context.Context) error {
	cmdCh := readLines(ctx)
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
				printOK("服务端已停止")
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
		printDim("服务端指令: list | ls [路径] | lst 用户id [路径] | ping 用户id | kick 用户id | tp 文件 用户id | tp -me 用户id 文件 | stop")
		return true
	case "stop":
		return false
	case "list":
		sh.listUsers()
		return true
	case "ls":
		// ls 列出服务端本地目录（保存目录）
		path := ""
		if len(fields) > 1 {
			path = fields[1]
		}
		sh.listLocal(path)
		return true
	case "lst":
		// lst 用户id [路径]: 列出该用户客户端的目录
		if len(fields) < 2 {
			printDim("用法: lst 用户id [路径]")
			return true
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			printErr("用户id 无效")
			return true
		}
		u := sh.userByID(id)
		if u == nil {
			printErr("用户 %d 不存在", id)
			return true
		}
		path := ""
		if len(fields) > 2 {
			path = fields[2]
		}
		sh.sendCtrl(u, ctrlMsg{Type: "ls_req", Name: path})
		return true
	case "ping":
		if len(fields) < 2 {
			printDim("用法: ping 用户id")
			return true
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			printErr("用户id 无效")
			return true
		}
		u := sh.userByID(id)
		if u == nil {
			printErr("用户 %d 不存在", id)
			return true
		}
		sh.sendCtrl(u, ctrlMsg{Type: "ping", Ts: time.Now().UnixNano()})
		return true
	case "kick":
		if len(fields) < 2 {
			printDim("用法: kick 用户id")
			return true
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			printErr("用户id 无效")
			return true
		}
		u := sh.userByID(id)
		if u == nil {
			printErr("用户 %d 不存在", id)
			return true
		}
		sh.sendCtrl(u, ctrlMsg{Type: "kick", Msg: "管理员将你踢出"})
		u.conn.Close()
		sh.unregisterUser(u)
		printOK("已踢出用户 %d", id)
		return true
	case "tp":
		return sh.execTp(fields)
	default:
		printErr("未知指令: %s（输入 help 查看）", fields[0])
		return true
	}
}

// listLocal 列出服务端保存目录（可带子路径）。
func (sh *serverShell) listLocal(path string) {
	dir := sh.rcv.dir
	if path != "" {
		if filepath.IsAbs(path) {
			printErr("路径无效: %s", path)
			return
		}
		dir = filepath.Join(dir, filepath.FromSlash(path))
	}
	printList("", listDir(dir))
}

// execTp 处理服务端 tp 指令: tp 文件 用户id / tp -me 用户id 文件。
func (sh *serverShell) execTp(fields []string) bool {
	me := false
	rest := fields[1:]
	if len(rest) > 0 && rest[0] == "-me" {
		me = true
		rest = rest[1:]
	}
	if len(rest) < 2 {
		printDim("用法: tp 文件 用户id 或 tp -me 用户id 文件")
		return true
	}
	var path, idStr string
	if me {
		// tp -me 用户id 文件: 先用户id, 后文件
		idStr, path = rest[0], rest[1]
	} else {
		path, idStr = rest[0], rest[1]
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		printErr("用户id 无效")
		return true
	}
	u := sh.userByID(id)
	if u == nil {
		printErr("用户 %d 不存在", id)
		return true
	}
	if me {
		// 拉取: 通知客户端把本地文件发到服务端
		sh.sendCtrl(u, ctrlMsg{Type: "send", Name: path})
		return true
	}
	// 推送: 通知客户端主动拉取（数据连接由客户端发起，NAT 下也能通）
	go sh.pushToUser(u, path)
	return true
}

// pushToUser 把服务端文件推送给指定用户。为避免服务端主动连接客户端
// （NAT/防火墙后连不通），改为通知客户端主动发起拉取连接。
func (sh *serverShell) pushToUser(u *user, path string) {
	if u.ver < pullProtoVer {
		printErr("用户 %d 的 tpfile 版本过旧，请对方更新到最新版后再试", u.id)
		return
	}
	full := path
	if !filepath.IsAbs(path) {
		// 相对路径基于服务端保存目录，与 ls 显示一致
		full = filepath.Join(sh.rcv.dir, path)
	}
	items, err := collectFiles([]string{full})
	if err != nil {
		printErr("发送 %s 给用户 %d 失败: %v", path, u.id, err)
		return
	}
	for _, it := range items {
		// 逐文件通知客户端拉取；客户端拉完会回报 pull_done
		sh.sendCtrl(u, ctrlMsg{Type: "pull", Name: filepath.ToSlash(it.rel), Size: it.size})
	}
	printOK("已通知用户 %d 拉取 %s (%d 个文件)", u.id, path, len(items))
}

// servePullChunk 响应客户端的一条拉取分块连接：校验路径后直接写出分块数据。
func (sh *serverShell) servePullChunk(conn net.Conn, h chunkHeader) {
	if h.Start < 0 || h.Len < 0 || h.Start+h.Len > h.Size {
		return
	}
	rel, err := sanitizeRelPath(h.Name)
	if err != nil {
		return
	}
	full := filepath.Join(sh.dir, filepath.FromSlash(rel))
	if !withinDir(sh.dir, full) {
		return
	}
	fi, err := os.Stat(full)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != h.Size {
		return
	}
	f, err := os.Open(full)
	if err != nil {
		return
	}
	defer f.Close()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Minute))
	if h.Len > 0 {
		if _, err := io.Copy(conn, io.NewSectionReader(f, h.Start, h.Len)); err != nil {
			return
		}
	}
	// 写完即关闭（由调用方 defer conn.Close() 处理），客户端读满即可
}

// withinDir 判断 path 是否位于 base 目录之内（防目录穿越）。
func withinDir(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
		printDim("当前没有用户连接")
		return
	}
	printDim("已连接的用户:")
	for _, id := range ids {
		u := byID[id]
		printInfo("用户 %d: %s (令牌 %s, 接收端口 %d)", id, u.ip, shortToken(u.token), u.inPort)
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
