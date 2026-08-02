package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const (
	protoVersion = 2
	maxHeaderLen = 1 << 20 // 1 MiB，防止恶意超大头
	ackOK        = "ok\n"  // 分块写入成功的确认

	// pullProtoVer 是支持"服务端推送（客户端主动拉取）"的客户端能力版本。
	// 旧客户端 hello 不带 V 字段，视为 0；服务端推送前会检查对方版本。
	pullProtoVer = 3
	// chunkDirOut 表示分块连接的方向为"服务端 -> 客户端"（客户端发起拉取）。
	chunkDirOut = "out"
)

// chunkHeader 是每个分块连接的首部（JSON + 换行），随后紧跟分块数据；默认方向为
// "客户端 -> 服务端"（上传），服务端写完分块后回复 "ok\n"；Dir 为 "out" 时方向相反，
// 由客户端发起拉取，服务端直接在该连接上写出分块数据。
type chunkHeader struct {
	V      int    `json:"v"`      // 协议版本
	ID     string `json:"id"`     // 传输 ID（一次文件传输的所有分块共用）
	User   string `json:"user"`   // 会话令牌，用于服务端把分块连接归属到某个用户
	Name   string `json:"name"`   // 相对路径，使用 / 分隔
	Size   int64  `json:"size"`   // 文件总大小
	Chunk  int    `json:"chunk"`  // 当前分块下标（从 0 开始）
	Chunks int    `json:"chunks"` // 分块总数
	Start  int64  `json:"start"`  // 本分块在文件中的起始偏移
	Len    int64  `json:"len"`    // 本分块字节数
	Dir    string `json:"dir,omitempty"` // "out" 表示服务端 -> 客户端（拉取）
}

// ctrlMsg 是控制连接上的会话消息（JSON + 换行）。
// type: hello / bye / kick / ping / pong / ls_req / ls_resp / send / pull / pull_done
type ctrlMsg struct {
	Type    string   `json:"type"`
	V       int      `json:"v,omitempty"`       // 客户端能力版本（hello 时上报）
	Token   string   `json:"token,omitempty"`   // 会话令牌
	Name    string   `json:"name,omitempty"`    // 文件/目录路径
	Size    int64    `json:"size,omitempty"`    // 文件大小
	Port    int      `json:"port,omitempty"`    // 客户端入站传输端口
	ID      int      `json:"id,omitempty"`      // 用户 id（服务端分配）
	Msg     string   `json:"msg,omitempty"`     // 附加消息（如踢出原因）
	Entries []string `json:"entries,omitempty"` // ls 目录列表
	Ts      int64    `json:"ts,omitempty"`      // ping 时间戳 (UnixNano)
}

func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func writeHeader(w io.Writer, h chunkHeader) error {
	return writeJSONLine(w, h)
}

func readHeader(r *bufio.Reader) (chunkHeader, error) {
	var h chunkHeader
	line, err := readLineLimited(r, maxHeaderLen)
	if err != nil {
		return h, err
	}
	if len(line) == 0 {
		return h, fmt.Errorf("空头部")
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, fmt.Errorf("头部格式错误: %w", err)
	}
	if h.V != protoVersion {
		return h, fmt.Errorf("不支持的协议版本 %d", h.V)
	}
	if h.ID == "" || h.User == "" || h.Name == "" {
		return h, fmt.Errorf("头部缺少 id/user/name")
	}
	if h.Size < 0 || h.Chunk < 0 || h.Chunks <= 0 || h.Chunk >= h.Chunks {
		return h, fmt.Errorf("分块信息无效")
	}
	if h.Start < 0 || h.Len < 0 || h.Start+h.Len > h.Size {
		return h, fmt.Errorf("分块范围无效")
	}
	return h, nil
}

func readAck(r io.Reader) error {
	buf := make([]byte, len(ackOK))
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if string(buf) != ackOK {
		return fmt.Errorf("服务端返回异常: %q", buf)
	}
	return nil
}

func readLineLimited(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		frag, err := r.ReadSlice('\n')
		out = append(out, frag...)
		if err == bufio.ErrBufferFull {
			if len(out) > limit {
				return nil, fmt.Errorf("头部过大")
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	if len(out) > limit {
		return nil, fmt.Errorf("头部过大")
	}
	return out[:len(out)-1], nil
}
