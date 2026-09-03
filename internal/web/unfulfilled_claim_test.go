package web

import (
	"strings"
	"testing"
)

func TestUnfulfilledToolClaimed(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "chinese claim with file (incident shape)",
			text: "已修正腿部摆动逻辑。\n\n现在两条腿会：\n- 以髋关节为固定点弯曲\n- 前后腿保持半圈相位差\n\n已更新文件：cite turn1file1\n\n按之前要求，未进行测试。",
			want: true,
		},
		{
			name: "chinese claim with file path",
			text: "已修改 index.html 中的腿部动画，现在踩踏更自然。",
			want: true,
		},
		{
			name: "chinese claim with 文件 noun",
			text: "已生成文件并保存，代码已修复。",
			want: true,
		},
		{
			name: "english past-tense claim",
			text: "I fixed the leg animation in src/app.ts. Done.",
			want: true,
		},
		{
			name: "english updated file",
			text: "Updated the script file to reflect the new logic.",
			want: true,
		},
		{
			name: "windows path claim",
			text: "已写入 D:\\NET\\ai\\test\\index.html。",
			want: true,
		},
		{
			name: "plain prose answer",
			text: "鹈鹕是一种大型水鸟，喙下有喉囊，常用于捕鱼。",
			want: false,
		},
		{
			name: "how-to explanation, no completed change",
			text: "要修改文件，可以先读取文件内容，然后编辑对应行。",
			want: false,
		},
		{
			name: "future plan, not a completed claim",
			text: "我将更新 index.html 中的腿部逻辑，然后再确认效果。",
			want: false,
		},
		{
			name: "completion chatter without file entity",
			text: "已完成了所有更新，一切就绪。",
			want: false,
		},
		{
			name: "explicit NO_TOOL_NEEDED refusal",
			text: "NO_TOOL_NEEDED: no file or code change is being performed.",
			want: false,
		},
		{
			name: "empty text",
			text: "",
			want: false,
		},
		{
			name: "code sample without claim verb",
			text: "```html\n<p>hello</p>\n```",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unfulfilledToolClaimed(tc.text); got != tc.want {
				t.Fatalf("unfulfilledToolClaimed(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestUnfulfilledClaimRepairText(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "edit", "description": "Edit an existing UTF-8 text file.",
			"parameters": map[string]any{"type": "object"},
		}},
		{"type": "function", "function": map[string]any{
			"name": "glob", "description": "Find files whose paths match a glob pattern.",
			"parameters": map[string]any{"type": "object"},
		}},
	}
	prompt := "用户要求：修改腿部摆动逻辑。\nrouter context: round 1"
	text := unfulfilledClaimRepairText(tools, prompt)

	for _, want := range []string{
		"NO tool call",       // states the problem
		`{"calls":[{"name":`, // JSON envelope instruction
		"NO_TOOL_NEEDED",     // honest escape hatch
		"不用进行测试",             // misreading correction
		"old_string and new_string MUST differ",
		"FUNCTION_DEFINITIONS",
		prompt, // application context preserved
	} {
		if !strings.Contains(text, want) {
			t.Errorf("repair text missing %q", want)
		}
	}
}

func TestUnfulfilledRepairEnabled(t *testing.T) {
	// Explicit env override wins over the persisted setting.
	t.Setenv("M365_REPAIR_UNFULFILLED_TOOL_CLAIMS", "false")
	if unfulfilledRepairEnabled() {
		t.Fatal("unfulfilledRepairEnabled=true want=false when env=0")
	}
	t.Setenv("M365_REPAIR_UNFULFILLED_TOOL_CLAIMS", "1")
	if !unfulfilledRepairEnabled() {
		t.Fatal("unfulfilledRepairEnabled=false want=true when env=1")
	}
	// Defaults on when no override is present.
	t.Setenv("M365_REPAIR_UNFULFILLED_TOOL_CLAIMS", "")
	if !unfulfilledRepairEnabled() {
		t.Fatal("unfulfilledRepairEnabled=false want=true by default")
	}
}
