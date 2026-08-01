package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// dialTarget 建立到目标的 TCP 连接；指定 proxy 时走 SOCKS5 代理。
func dialTarget(proxy, addr string) (net.Conn, error) {
	if proxy != "" {
		return socks5Dial(proxy, addr)
	}
	return net.DialTimeout("tcp", addr, 10*time.Second)
}

// socks5Dial 通过 SOCKS5 代理建立 CONNECT 连接（无认证，兼容 Clash 等默认配置）。
func socks5Dial(proxy, target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("端口无效: %q", portStr)
	}

	conn, err := net.DialTimeout("tcp", proxy, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接代理 %s 失败: %w", proxy, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	// 握手：版本 5，方法=无认证
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	if resp[0] != 5 {
		return nil, fmt.Errorf("代理返回非 SOCKS5 版本 %d", resp[0])
	}
	if resp[1] != 0 {
		return nil, fmt.Errorf("代理要求认证（方法 %d），暂不支持", resp[1])
	}

	// CONNECT 请求
	req := []byte{5, 1, 0}
	ip := net.ParseIP(host)
	switch {
	case ip != nil && ip.To4() != nil:
		req = append(req, 1) // IPv4
		req = append(req, ip.To4()...)
	case ip != nil:
		req = append(req, 4) // IPv6
		req = append(req, ip.To16()...)
	default:
		if len(host) > 255 {
			return nil, fmt.Errorf("域名过长")
		}
		req = append(req, 3, byte(len(host))) // 域名
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	if head[0] != 5 {
		return nil, fmt.Errorf("代理返回非 SOCKS5 版本")
	}
	if head[1] != 0 {
		return nil, fmt.Errorf("代理连接目标失败，错误码 %d", head[1])
	}
	var addrLen int
	switch head[3] {
	case 1:
		addrLen = 4
	case 4:
		addrLen = 16
	case 3:
		b := make([]byte, 1)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		addrLen = int(b[0])
	default:
		return nil, fmt.Errorf("代理返回未知地址类型 %d", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addrLen+2)); err != nil {
		return nil, err
	}
	ok = true
	return conn, nil
}
