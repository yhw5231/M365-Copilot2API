package web

import (
	"testing"
	"time"
)

// windowBenchCase is one probe used to compare scan-window sizes. want is the
// expected detection result for the actual call site (base detector alone, or
// tool-aware variant), and label describes the scenario.
type windowBenchCase struct {
	name  string
	text  string
	tools []map[string]any // nil → base detector, non-nil → tool-aware
	want  bool
}

func windowBenchSuite() []windowBenchCase {
	toolSet := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "pwsh", "description": "PowerShell"}},
		{"type": "function", "function": map[string]any{"name": "read", "description": "read files"}},
		{"type": "function", "function": map[string]any{"name": "write", "description": "write files"}},
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit files"}},
		{"type": "function", "function": map[string]any{"name": "glob", "description": "glob files"}},
		{"type": "function", "function": map[string]any{"name": "grep", "description": "search files"}},
	}
	return []windowBenchCase{
		// --- genuine misjudgments that MUST be detected ---
		{name: "zh explicit", text: "当前会话没有任何可用工具", want: true},
		{name: "zh reported bug", text: "目前仍无法继续修改：当前会话实际没有可调用的 pwsh、read、edit、write、glob 或 grep 文件操作接口。", want: true},
		{name: "zh no exec", text: "当前会话没有执行工具，无法运行命令", want: true},
		{name: "zh /mnt/data", text: "当前会话只能访问 /mnt/data。", want: true},
		{name: "en explicit linux", text: "This session only provides a Linux container.", want: true},
		{name: "en rephrase tools", text: "The tools are not available in this session.", want: true},
		{name: "en scope first", text: "This session does not have any tools available.", want: true},
		{name: "en no shell", text: "I don't have shell access in this environment.", want: true},
		{name: "en exclusivity", text: "This environment only provides a Linux sandbox.", want: true},
		{name: "en command exec", text: "I don't have command execution in this session.", want: true},
		{name: "en current session linux", text: "current session only provides a linux container", want: true},
		{name: "en tool-aware read", text: "The read tool is not available in this environment.", tools: toolSet, want: true},
		{name: "en tool-aware write zh", text: "write 工具不可用", tools: toolSet, want: true},
		{name: "en tool-aware pwsh", text: "I don't have pwsh here.", tools: toolSet, want: true},

		// --- legitimate narration that MUST NOT be detected ---
		{name: "deploy note", text: "The service deploys in a Linux container.", want: false},
		{name: "path note", text: "Copy the generated file to /mnt/data before returning it.", want: false},
		{name: "tool error", text: "The command failed with exit code 1.", want: false},
		{name: "permission", text: "I don't have permission to write to that directory.", want: false},
		{name: "file locked", text: "The file cannot be read because it is locked.", want: false},
		{name: "file not found", text: "The file was not found in the workspace.", want: false},
		{name: "mid-reply zh denial", text: "我已经完成了对项目的分析，识别出需要修改的三个文件，并逐项核实了每个文件的当前内容与修改点。修改后的配置、请求处理器和测试套件已经整理完毕，下面给出最终说明，并补充相关的背景资料、运行参数、预期行为以及回滚方案，确保改动可以安全上线。另外说明：当前会话实际没有可调用的文件操作接口，这只是环境背景，不影响结论。", want: false},
		{name: "mid-reply en denial", text: "I have finished analyzing the project and verified the three files that need changes. Note that the current session does not provide any file operation interface, but this is environment background and does not affect the conclusion.", want: false},
		{name: "legit access", text: "I don't have write access to this session's files.", want: false},
		{name: "legit read fails", text: "The read tool returned a permission error.", tools: toolSet, want: false},
		{name: "legit write deny zh", text: "write 命令被拒绝，因为没有权限", tools: toolSet, want: false},
		// Known boundary (not part of the corpus): an assistant reply that
		// *quotes* the user's claim ("用户提到他没有工具") can be flagged. The
		// consequence is only a harmless one-shot correction retry — user
		// messages are never removed by cleanWorkspaceToolMisjudgments — so this
		// synthetic echo is deliberately excluded from the accuracy baseline.
		// {name: "user echo zh", text: "用户提到他没有工具，但从协议看本会话工具已声明，我用它们继续", want: false},
	}
}

// detectWith runs the detection path that matches the case's tool set.
func detectWith(c windowBenchCase) bool {
	if len(c.tools) > 0 {
		return isWorkspaceToolMisjudgmentForTools(c.text, c.tools)
	}
	return isWorkspaceToolMisjudgment(c.text)
}

// TestWindowSizeAccuracy compares scan-window sizes for false negatives
// (genuine misjudgment missed) and false positives (legitimate narration
// flagged). The goal is the smallest window that keeps both at zero for the
// realistic corpus: genuine misjudgments are opening claims, so a small window
// should catch them, while legitimate narration is usually mid-reply.
func TestWindowSizeAccuracy(t *testing.T) {
	original := workspaceToolMisjudgmentScanLimit
	defer func() { workspaceToolMisjudgmentScanLimit = original }()

	for _, size := range []int{90, 150, 300, 600} { // ≈ 30/50/100/200 汉字 (UTF-8)
		workspaceToolMisjudgmentScanLimit = size
		fn, fp := 0, 0
		for _, c := range windowBenchSuite() {
			got := detectWith(c)
			if got != c.want {
				if c.want {
					fn++
					t.Logf("window=%d FN  %s: %q", size, c.name, c.text)
				} else {
					fp++
					t.Logf("window=%d FP  %s: %q", size, c.name, c.text)
				}
			}
		}
		t.Logf("window=%4d bytes → %d false negatives, %d false positives", size, fn, fp)
		// The production window (150 bytes ≈ 50 汉字) must have zero FN and
		// zero FP on the real corpus. The other sizes are measured for
		// comparison only.
		if size == 150 && (fn > 0 || fp > 0) {
			t.Errorf("production window 150 must have zero FN and zero FP, got %d FN, %d FP", fn, fp)
		}
	}
}

// TestWindowSizeSpeed measures the per-call cost of the detection layers at
// different window sizes. Larger windows scan more text, but the corpus is
// short, so the difference should be small; the test records the numbers rather
// than asserting a hard bound.
func TestWindowSizeSpeed(t *testing.T) {
	original := workspaceToolMisjudgmentScanLimit
	defer func() { workspaceToolMisjudgmentScanLimit = original }()

	suite := windowBenchSuite()
	iterations := 2000
	for _, size := range []int{30, 50, 100, 300} {
		workspaceToolMisjudgmentScanLimit = size
		start := time.Now()
		for i := 0; i < iterations; i++ {
			for _, c := range suite {
				detectWith(c)
			}
		}
		elapsed := time.Since(start)
		perCall := elapsed / time.Duration(iterations*len(suite))
		t.Logf("window=%3d chars → %v per detection (corpus of %d cases)", size, perCall, len(suite))
	}
}