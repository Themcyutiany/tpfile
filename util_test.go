package main

import "testing"

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in   string
		port int
		want string
		ok   bool
	}{
		{"192.168.1.5:1090", 1090, "192.168.1.5:1090", true},
		{"192.168.1.5", 1090, "192.168.1.5:1090", true},
		{"[::1]:1090", 1090, "[::1]:1090", true},
		{"::1", 1090, "[::1]:1090", true},
		{"::1:1090", 1090, "[::1]:1090", true},
		{"example.com:8080", 1090, "example.com:8080", true},
		{"example.com", 1090, "example.com:1090", true},
		{"localhost", 7777, "localhost:7777", true},
		{"", 1090, "", false},
		{"1.2.3.4:99999", 1090, "", false},
		{"[::1", 1090, "", false},
	}
	for _, c := range cases {
		got, err := parseTarget(c.in, c.port)
		if c.ok != (err == nil) || (err == nil && got != c.want) {
			t.Errorf("parseTarget(%q, %d) = %q, %v; want %q", c.in, c.port, got, err, c.want)
		}
	}
}

func TestSanitizeRelPath(t *testing.T) {
	valid := map[string]string{
		"a.txt":       "a.txt",
		"dir/a.txt":   "dir/a.txt",
		`dir\a.txt`:   "dir/a.txt",
		"/etc/passwd": "etc/passwd",
		"./a/./b.txt": "a/b.txt",
		"a//b":        "a/b",
	}
	for in, want := range valid {
		got, err := sanitizeRelPath(in)
		if err != nil || got != want {
			t.Errorf("sanitizeRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	invalid := []string{"../x", "a/../b", "C:/x", "a:b", `a\..\b`, ""}
	for _, in := range invalid {
		if _, err := sanitizeRelPath(in); err == nil {
			t.Errorf("sanitizeRelPath(%q) 应该失败", in)
		}
	}
}

func TestChunkPlan(t *testing.T) {
	for _, size := range []int64{0, 1, 1000, 256 << 10, 1 << 20, 1<<20 + 1, 10 << 20} {
		chunks := chunkPlan(size, 4)
		if size == 0 {
			if len(chunks) != 1 || chunks[0].len != 0 {
				t.Fatalf("size 0: %v", chunks)
			}
			continue
		}
		var sum int64
		var prev int64
		for _, c := range chunks {
			if c.start != prev {
				t.Fatalf("size %d: 分块不连续，偏移 %d", size, c.start)
			}
			if c.len <= 0 {
				t.Fatalf("size %d: 空分块", size)
			}
			sum += c.len
			prev = c.start + c.len
		}
		if sum != size {
			t.Fatalf("size %d: 覆盖字节数 %d", size, sum)
		}
	}
	if n := len(chunkPlan(10<<20, 4)); n != 4 {
		t.Fatalf("10MB 应拆 4 块, got %d", n)
	}
	if n := len(chunkPlan(1<<20, 4)); n != 4 {
		t.Fatalf("1MB 应拆 4 块, got %d", n)
	}
	if n := len(chunkPlan(100<<10, 8)); n != 1 {
		t.Fatalf("100KB 应拆 1 块, got %d", n)
	}
}
