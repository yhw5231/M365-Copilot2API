package web

import (
	"strings"
	"testing"
)

func TestIsBareNoToolMarker(t *testing.T) {
	marker := "NO_TOOL_NEEDED: no file or code change is being performed"
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"exact marker", marker, true},
		{"marker with trailing period", marker + ".", true},
		{"lowercase variant", "no_tool_needed: no file or code change is being performed", true},
		{"bare token only", "NO_TOOL_NEEDED", true},
		{"token lowercase only", "no_tool_needed", true},
		{"surrounding blank lines", "\n\n" + marker + "\n", true},
		{"empty", "", false},
		{"whitespace only", "  \n\t ", false},
		{"real answer", "已修正 index.html，腿部动画已与脚蹬同步。", false},
		{"real answer mentioning marker", "我认为无需调用工具，原因如下：\n" + marker + " 这条不是我的回答。", false},
		{"marker plus progress note", marker + "\n我已经检查过文件内容，确认无需改动。", false},
		{"normal english answer", "The file is already correct; no change was needed.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBareNoToolMarker(c.text); got != c.want {
				t.Fatalf("isBareNoToolMarker(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

func TestNoToolMarkerRecoveryText(t *testing.T) {
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "edit", "description": "edit a file"}},
	}
	text := noToolMarkerRecoveryText(tools, "fix the leg animation")
	for _, want := range []string{"no file or code change is being performed", `"calls"`, "do NOT repeat", "fix the leg animation"} {
		if !strings.Contains(text, want) {
			t.Fatalf("recovery text missing %q:\n%s", want, text)
		}
	}
}
