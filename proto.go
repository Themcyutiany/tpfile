package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

const (
	protoVersion = 1
	maxHeaderLen = 1 << 20 // 1 MiB，防止恶意超大头
	ackOK        = "ok\n"  // 分块写入成功的确认
)

// chunkHeader 是每个分块连接的首部（JSON + 换行），随后紧跟分块数据，服务端写完分块后回复 "ok\n"。
type chunkHeader struct {
	V      int    `json:"v"`      // 协议版本
	ID     string `json:"id"`     // 传输 ID（一次文件传输的所有分块共用）
	Name   string `json:"name"`   // 相对路径，使用 / 分隔
	Size   int64  `json:"size"`   // 文件总大小
	Chunk  int    `json:"chunk"`  // 当前分块下标（从 0 开始）
	Chunks int    `json:"chunks"` // 分块总数
	Start  int64  `json:"start"`  // 本分块在文件中的起始偏移
	Len    int64  `json:"len"`    // 本分块字节数
}

func writeHeader(w io.Writer, h chunkHeader) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
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
	if h.ID == "" || h.Name == "" {
		return h, fmt.Errorf("头部缺少 id/name")
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
