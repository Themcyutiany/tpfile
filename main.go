package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

const version = "1.2.0"

var (
	outMu        sync.Mutex
	progressLive bool // 终端下是否有未换行的进度条
)

// printLine 串行化输出；若终端上还挂着进度条，先清掉再打印日志。
func printLine(format string, args ...any) {
	outMu.Lock()
	defer outMu.Unlock()
	if progressLive {
		fmt.Fprint(os.Stdout, "\r"+strings.Repeat(" ", 100)+"\r")
		progressLive = false
	}
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func usage() {
	fmt.Fprint(flag.CommandLine.Output(), `tpfile - 多线程文件传输工具 (v`+version+`)

用法:
  tpfile -s [-p 端口] [-d 保存目录]                  # 服务端: 监听并接收文件
  tpfile -c 主机:端口 [-f 文件或目录] [-t 线程数] [-j 并行数]  # 客户端: 发送文件
  tpfile -c [::1]:1090 -f a.bin --proxy 127.0.0.1:7897

参数:
  -s, --serve            监听模式（接收文件）
  -c, --connect 地址     连接模式，如 192.168.1.5:1090 或 [::1]:1090（不带端口时默认 1090，可用 -p 覆盖）
  -p, --port 端口        端口，默认 1090
  -d, --dir 目录         服务端保存目录，默认当前目录
  -f, --file 路径        要发送的文件或目录，可重复；目录会递归发送
  -t, --threads 数量     每个文件的并行连接数，默认 4
  -j, --jobs 数量        同时并行发送的文件数（目录传输更快），默认 4
  -r, --retries 数量     分块失败重试次数，默认 3
      --proxy 地址       出站走 SOCKS5 代理，如 127.0.0.1:7897
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
		files   stringList
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
	flag.Var(&files, "f", "")
	flag.Var(&files, "file", "")
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

	var err error
	if serve {
		err = runServer(ctx, port, dir, verbose)
	} else {
		addr, perr := parseTarget(connect, port)
		if perr != nil {
			fail("%v", perr)
		}
		if len(files) == 0 {
			fail("客户端模式需要 -f 指定要发送的文件或目录")
		}
		if proxy != "" {
			printLine("使用 SOCKS5 代理 %s", proxy)
		}
		err = runClient(ctx, addr, files, proxy, threads, retries, jobs, verbose)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}
