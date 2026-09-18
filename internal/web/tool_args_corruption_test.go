package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Example taken from the real DSH session that corrupted the goal step
// "执行构建、测试与容器配置验证": the tool-call arguments were ~750 bytes of
// UTF-8 Chinese JSON, the 512-byte window fell inside 证 (E8 AF 81), and the
// following fragment carried orphan continuation bytes that JSON encoding
// turned into U+FFFD — the goal list permanently read "验�证" in the session.
func TestToolArgsChunksRuneSafe(t *testing.T) {
	sizes := []int{1, 2, 3, 7, 511, 512, 513, 700, 100000}
	args := `{"todos":[{"content":"检查当前工作目录、项目结构及现有 Git 状态","status":"completed"},{"content":"执行构建、测试与容器配置验证","status":"pending"},{"content":"检查最终变更并完成目标","status":"pending"}]}`
	for _, size := range sizes {
		chunks := toolArgsChunks(args, size)
		if len(chunks) == 0 {
			t.Fatalf("size=%d: no chunks", size)
		}
		var joined strings.Builder
		for i, c := range chunks {
			if !utf8.ValidString(c) {
				t.Fatalf("size=%d chunk %d is invalid UTF-8: %q", size, i, c)
			}
			if len(c) == 0 && i < len(chunks)-1 {
				t.Fatalf("size=%d chunk %d empty", size, i)
			}
			joined.WriteString(c)
		}
		if joined.String() != args {
			t.Fatalf("size=%d: reassembled arguments differ from the original", size)
		}
	}
}

// The corruption appeared only when the boundary landed in the middle of a
// Chinese character; the fixed-offset loop (off += size) restarted the next
// fragment at an offset that was not rune-aligned. Every reported variant
// ("验�证", "验证��", a duplicated "," quote pair) traces to that single cut.
func TestToolArgsChunksBoundaryInsideChineseChar(t *testing.T) {
	// Pad so the 512-byte window falls on the last continuation byte of 验
	// (E9 AA 8C) in "验证" — the exact cut that produced 验�证 in the DSH
	// session. The old loop restarted the next fragment at byte 512, whose
	// leading orphan continuation byte was invalid UTF-8.
	args := strings.Repeat("a", 463) + `"content":"执行构建、测试与容器配置验证","status":"pending"}`
	chunks := toolArgsChunks(args, 512)
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d invalid: %q", i, c)
		}
		if strings.ContainsRune(c, '\uFFFD') {
			t.Fatalf("chunk %d contains U+FFFD: %q", i, c)
		}
	}
	if got := strings.Join(chunks, ""); got != args {
		t.Fatal("reassembled arguments differ from the original")
	}
	// The corrupted text must never appear anywhere in the stream.
	if strings.Contains(strings.Join(chunks, ""), "\uFFFD") {
		t.Fatal("stream contains U+FFFD")
	}
}

func TestRuneSafeTruncate(t *testing.T) {
	long := "执行构建、测试与容器配置验证" // 6 × 3 bytes = 18 bytes
	trunc := runeSafeTruncate(long, 10)
	if !utf8.ValidString(trunc) {
		t.Fatalf("truncated text invalid UTF-8: %q", trunc)
	}
	if len(trunc) > 10 {
		t.Fatalf("truncated length %d > 10", len(trunc))
	}
	if !strings.HasPrefix(long, trunc) {
		t.Fatalf("truncation not a prefix: %q vs %q", trunc, long)
	}
	if strings.ContainsRune(trunc, '\uFFFD') {
		t.Fatalf("truncated text contains U+FFFD: %q", trunc)
	}
	if got := runeSafeTruncate(long, len(long)); got != long {
		t.Fatalf("limit == len must return the input untouched")
	}
	if got := runeSafeTruncate("abc", 2); got != "ab" {
		t.Fatalf("plain ASCII cut: got %q", got)
	}
}

// compactToolResult feeds the goal ledger into the router prompt; a
// mid-character byte cut there would let the model see (and echo) a U+FFFD.
func TestCompactToolResultRuneSafe(t *testing.T) {
	long := strings.Repeat("执行构建、测试与容器配置验证", 40)
	out := compactToolResult(long, 600)
	if !utf8.ValidString(out) {
		t.Fatalf("compactToolResult output invalid UTF-8")
	}
	if strings.ContainsRune(out, '\uFFFD') {
		t.Fatalf("compactToolResult output contains U+FFFD")
	}
	if len(out) > 600+128 {
		t.Fatalf("compactToolResult output unexpectedly large: %d", len(out))
	}
	if !strings.Contains(out, "截断") && !strings.Contains(out, "truncated") {
		t.Fatalf("expected a truncation marker for long input")
	}
}
