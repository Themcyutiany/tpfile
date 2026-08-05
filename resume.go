package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"
)

// resumeDirName 是接收目录下存放断点续传位图的隐藏目录。
const resumeDirName = ".tpfile-resume"

// resumeInfo 是单个文件传输的续传位图（持久化在接收端）。
type resumeInfo struct {
	Size   int64  `json:"size"`   // 文件总大小（不匹配时位图作废）
	Chunks []bool `json:"chunks"` // 已完整落盘的分块（与分块下标一一对应）
}

// resumeFilePath 返回某个文件续传位图的保存路径（按文件名哈希，避免路径问题）。
func resumeFilePath(dir, name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(dir, resumeDirName, hex.EncodeToString(sum[:])+".json")
}

// loadResumeBits 读取接收目录下的续传位图。文件不存在、损坏或大小不匹配时返回
// (nil, false)，调用方按全量传输处理。
func loadResumeBits(dir, name string, size int64) ([]bool, bool) {
	p := resumeFilePath(dir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var ri resumeInfo
	if err := json.Unmarshal(b, &ri); err != nil || ri.Size != size {
		return nil, false
	}
	if len(ri.Chunks) == 0 {
		return nil, false
	}
	return ri.Chunks, true
}

// saveResumeBits 保存续传位图（原子写入：先写临时文件再重命名）。
func saveResumeBits(dir, name string, size int64, chunks []bool) error {
	dirPath := filepath.Join(dir, resumeDirName)
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		return err
	}
	p := resumeFilePath(dir, name)
	b, err := json.Marshal(resumeInfo{Size: size, Chunks: chunks})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// clearResumeBits 文件完整接收后删除位图。
func clearResumeBits(dir, name string) {
	os.Remove(resumeFilePath(dir, name))
}

// sanitizeResume 校验位图里的分块是否仍然有效：分块末尾超出当前文件大小的视为失效
// （文件可能被截断或删除后重建）。
func sanitizeResume(bits []bool, size int64, fileSize int64) []bool {
	if fileSize < 0 {
		fileSize = 0
	}
	chunks := chunkPlan(size, len(bits))
	out := make([]bool, len(bits))
	for i, ok := range bits {
		if !ok || i >= len(chunks) {
			continue
		}
		if chunks[i].start+chunks[i].len <= fileSize {
			out[i] = true
		}
	}
	return out
}

// queryResume 发送端向接收端查询已存在分块的位图（走一条普通数据连接）。
// 旧版接收端不认识该消息会直接断开连接，此时返回 nil 表示按全量传输。
func queryResume(dial func() (net.Conn, error), token, name string, size int64, nChunks int) []bool {
	conn, err := dial()
	if err != nil {
		return nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := writeJSONLine(conn, resumeQuery{Type: resumeQueryType, V: protoVersion, User: token, Name: name, Size: size, Chunks: nChunks}); err != nil {
		return nil
	}
	br := bufio.NewReader(conn)
	line, err := readLineLimited(br, maxHeaderLen)
	if err != nil {
		return nil
	}
	var rp resumeReply
	if err := json.Unmarshal(line, &rp); err != nil || len(rp.Chunks) != nChunks {
		return nil
	}
	return rp.Chunks
}

// replyResumeQuery 接收端处理一条续传查询连接：返回本地位图后关闭。
func replyResumeQuery(conn net.Conn, r *receiver, name string, size int64, nChunks int) {
	defer conn.Close()
	bits, _ := loadResumeBits(r.dir, name, size)
	if len(bits) != nChunks {
		bits = make([]bool, nChunks)
	} else if fi, err := os.Stat(filepath.Join(r.dir, filepath.FromSlash(name))); err == nil {
		bits = sanitizeResume(bits, size, fi.Size())
	}
	conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	writeJSONLine(conn, resumeReply{Chunks: bits})
}
