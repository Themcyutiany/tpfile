package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const version = "1.5.0"

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `tpfile - 局域网交互式文件传输工具 (v`+version+`)

用法:
  tpfile -s [-p 端口] [-d 保存目录]           服务端: 监听端口, 管理用户会话
  tpfile -c 主机:端口                         客户端: 连接服务端, 进入交互会话

交互指令（每行前都有 > 提示符）:
  客户端:
    tp 文件或文件夹             发送本地文件/目录到服务端
    tp -me 服务端文件           从服务端下载文件到本地
    ls [路径]                   查看本地当前目录（Linux ls 风格）
    lst [路径]                  查看服务端当前目录
    ping                        测试与服务端的延迟
    stop / Ctrl+C               断开连接
  服务端:
    list                        列出已连接的用户
    ls [路径]                   查看服务端本地目录（保存目录）
    lst 用户id [路径]           查看该用户客户端的当前目录
    ping 用户id                 测试与该用户的延迟
    kick 用户id                 踢出该用户
    tp 文件 用户id              发送服务端文件到该用户
    tp -me 用户id 文件          从该用户客户端拉取文件
    stop / Ctrl+C               停止服务

交互提示:
  > 提示符支持 Tab 补全本地文件路径、左右方向键移动光标

参数:
  -s, --serve            服务端模式
  -c, --connect 地址     连接模式，如 192.168.1.5:1090 或 [::1]:1090
  -p, --port 端口        端口，默认 1090
  -d, --dir 目录         服务端保存目录，默认当前目录
  -t, --threads 数量     每个文件的并行连接数，默认 4
  -j, --jobs 数量        同时并行传输的文件数，默认 4
  -r, --retries 数量     分块失败重试次数，默认 3
      --proxy 地址       客户端出站走 SOCKS5 代理，如 127.0.0.1:7897
  -v, --verbose          显示详细日志
      --version          显示版本
  -h, --help             显示帮助
`)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func main() {
	flag.Usage = usage

	var (
		serve   bool
		connect string
		port    int
		dir     string
		threads int
		retries int
		jobs    int
		proxy   string
		verbose bool
		showVer bool
	)

	flag.BoolVar(&serve, "s", false, "")
	flag.BoolVar(&serve, "serve", false, "")
	flag.StringVar(&connect, "c", "", "")
	flag.StringVar(&connect, "connect", "", "")
	flag.IntVar(&port, "p", 1090, "")
	flag.IntVar(&port, "port", 1090, "")
	flag.StringVar(&dir, "d", "", "")
	flag.StringVar(&dir, "dir", "", "")
	flag.IntVar(&threads, "t", 4, "")
	flag.IntVar(&threads, "threads", 4, "")
	flag.IntVar(&jobs, "j", 4, "")
	flag.IntVar(&jobs, "jobs", 4, "")
	flag.IntVar(&retries, "r", 3, "")
	flag.IntVar(&retries, "retries", 3, "")
	flag.StringVar(&proxy, "proxy", "", "")
	flag.BoolVar(&verbose, "v", false, "")
	flag.BoolVar(&verbose, "verbose", false, "")
	flag.BoolVar(&showVer, "version", false, "")
	flag.Parse()

	if showVer {
		fmt.Printf("tpfile %s\n", version)
		return
	}
	if serve && connect != "" {
		fail("不能同时使用 -s 和 -c")
	}
	if !serve && connect == "" {
		fail("需要指定 -s（服务端）或 -c 主机:端口（客户端）")
	}
	if port < 1 || port > 65535 {
		fail("端口必须在 1-65535 之间")
	}
	if threads < 1 {
		threads = 1
	}
	if jobs < 1 {
		jobs = 1
	}
	if retries < 0 {
		retries = 0
	}
	if dir == "" {
		dir = "."
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer setRawMode(false)

	var err error
	if serve {
		err = runServerInteractive(ctx, port, dir, threads, retries, jobs, verbose)
	} else {
		addr, perr := parseTarget(connect, port)
		if perr != nil {
			fail("%v", perr)
		}
		if proxy != "" {
			printLine("使用 SOCKS5 代理 %s", proxy)
		}
		err = runClientInteractive(ctx, addr, proxy, threads, retries, jobs, verbose)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}
