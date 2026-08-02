//go:build windows

package main

import (
	"bytes"
	"testing"
)

// TestKeySeqFromVK 验证 Windows 特殊键到终端转义序列的翻译。
func TestKeySeqFromVK(t *testing.T) {
	cases := []struct {
		vk    uint16
		state uint32
		want  []byte
	}{
		{vkLeft, 0, []byte("\x1b[D")},
		{vkRight, 0, []byte("\x1b[C")},
		{vkUp, 0, []byte("\x1b[A")},
		{vkDown, 0, []byte("\x1b[B")},
		{vkHome, 0, []byte("\x1b[H")},
		{vkEnd, 0, []byte("\x1b[F")},
		{vkDelete, 0, []byte("\x1b[3~")},
		{vkBack, 0, []byte{0x7f}},
		{vkTab, 0, []byte{0x09}},
		{vkReturn, 0, []byte{0x0d}},
		{vkEscape, 0, []byte{0x1b}},
		{'C', leftCtrl, []byte{0x03}},
		{'D', rightCtrl, []byte{0x04}},
		{'Z', leftCtrl, []byte{0x1a}},
		{0x70, 0, nil}, // F1 不产生输入
	}
	for _, c := range cases {
		got := keySeqFromVK(c.vk, c.state)
		if !bytes.Equal(got, c.want) {
			t.Errorf("keySeqFromVK(0x%X, 0x%X) = %v, want %v", c.vk, c.state, got, c.want)
		}
	}
}

// TestKeyCharBytes 验证 UTF-16 码元转 UTF-8（含代理对）。
func TestKeyCharBytes(t *testing.T) {
	var high uint16
	// 普通 BMP 字符
	if got := keyCharBytes('a', &high); string(got) != "a" {
		t.Fatalf("普通字符: %q", got)
	}
	// 中文（3 字节 UTF-8）
	if got := keyCharBytes('中', &high); string(got) != "中" {
		t.Fatalf("中文: %q", got)
	}
	// 代理对（U+1F600）
	if got := keyCharBytes(0xD83D, &high); got != nil {
		t.Fatalf("高代理项应暂存: %q", got)
	}
	if got := keyCharBytes(0xDE00, &high); string(got) != "\U0001F600" {
		t.Fatalf("代理对: %q", got)
	}
	// 非法低代理项 → 替换符
	if got := keyCharBytes(0xD83D, &high); got != nil {
		t.Fatalf("高代理项应暂存: %q", got)
	}
	if got := keyCharBytes('x', &high); string(got) != "\uFFFD" {
		t.Fatalf("非法代理对: %q", got)
	}
}
