package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// parseTarget 解析 "主机:端口"，兼容 IPv6（[::1]:1090 / ::1 / ::1:1090）。
// 地址中未带端口时使用 defPort。
// listDir 返回目录条目列表（目录名带 / 后缀，已排序）。
func listDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"<读取失败: " + err.Error() + ">"}
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// listPathEntries 列出 base 下（或子路径 path 下）的目录条目。
// path 为空表示 base 本身；绝对路径会被拒绝。
func listPathEntries(base, path string) []string {
	if path == "" {
		return listDir(base)
	}
	if filepath.IsAbs(path) {
		return []string{"<路径无效>"}
	}
	return listDir(filepath.Join(base, filepath.FromSlash(path)))
}

// fmtRTT 把往返延迟格式化成可读文本（<1ms 用微秒，其余用毫秒）。
func fmtRTT(d time.Duration) string {
	switch {
	case d < 0:
		return "异常"
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return ">60s"
	}
}

// parseTarget 解析 "主机:端口"，兼容 IPv6（[::1]:1090 / ::1 / ::1:1090）。
func parseTarget(addr string, defPort int) (string, error) {
	if defPort < 1 || defPort > 65535 {
		return "", fmt.Errorf("端口无效: %d", defPort)
	}
	if addr == "" {
		return "", fmt.Errorf("地址为空")
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if port == "" {
			port = strconv.Itoa(defPort)
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return "", fmt.Errorf("端口无效: %q", port)
		}
		return net.JoinHostPort(host, port), nil
	}
	// 兼容 "::1:1090" 这种未加方括号的 IPv6 写法（最后一段冒号视为端口）。
	// 注意要先于 ParseIP 判断：net.ParseIP("::1:1090") 会把它当成合法 IPv6 地址。
	if i := strings.LastIndexByte(addr, ':'); i > 0 && i < len(addr)-1 {
		host, port := addr[:i], addr[i+1:]
		if p, err := strconv.Atoi(port); err == nil && p >= 1 && p <= 65535 {
			if net.ParseIP(host) != nil || !strings.Contains(host, ":") {
				return net.JoinHostPort(host, strconv.Itoa(p)), nil
			}
		}
	}
	if ip := net.ParseIP(addr); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(defPort)), nil
	}
	if !strings.Contains(addr, ":") {
		return net.JoinHostPort(addr, strconv.Itoa(defPort)), nil
	}
	return "", fmt.Errorf("无法解析地址 %q（格式: 主机:端口，如 192.168.1.5:1090 或 [::1]:1090）", addr)
}

// sanitizeRelPath 把收到的文件名清洗成安全的相对路径，防止目录穿越。
func sanitizeRelPath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	parts := strings.Split(name, "/")
	var out []string
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			return "", fmt.Errorf("路径包含非法组件 '..'")
		}
		if strings.ContainsRune(p, ':') {
			return "", fmt.Errorf("路径包含非法组件 %q", p)
		}
		if strings.IndexByte(p, 0) >= 0 {
			return "", fmt.Errorf("路径包含非法字符")
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("路径为空")
	}
	return strings.Join(out, "/"), nil
}

// uniqueOpen 以排他方式创建文件；同名时自动追加 (1)、(2)…
func uniqueOpen(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return f, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("无法为 %s 生成不重复的文件名", base)
}

// chunk 表示文件的一个分块。
type chunk struct {
	start int64
	len   int64
}

// chunkPlan 把文件切成若干分块：小文件用较少连接，大文件按线程数并行。
func chunkPlan(size int64, threads int) []chunk {
	if threads < 1 {
		threads = 1
	}
	if size <= 0 {
		return []chunk{{start: 0, len: 0}}
	}
	const minChunk = 256 << 10 // 256 KiB
	n := threads
	if size < int64(n)*minChunk {
		n = int((size + minChunk - 1) / minChunk)
		if n < 1 {
			n = 1
		}
	}
	chunkSize := (size + int64(n) - 1) / int64(n)
	var chunks []chunk
	for off := int64(0); off < size; off += chunkSize {
		l := chunkSize
		if off+l > size {
			l = size - off
		}
		chunks = append(chunks, chunk{start: off, len: l})
	}
	return chunks
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func humanSize(n int64) string {
	f := float64(n)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.2f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.2f KB", f/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func humanRate(f float64) string {
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.2f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.2f KB", f/(1<<10))
	default:
		return fmt.Sprintf("%.0f B", f)
	}
}

func etaStr(d time.Duration) string {
	s := int(d.Seconds())
	if s < 0 {
		s = 0
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}
