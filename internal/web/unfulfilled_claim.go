package web

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// unfulfilledRepairEnabled reports whether the gateway runs the soft repair
// that catches answers claiming a completed file/code change while the round
// carried no tool call at all. It is ON by default. Precedence: an explicit
// M365_REPAIR_UNFULFILLED_TOOL_CLAIMS environment variable wins, then the
// persisted runtime setting (repairUnfulfilledToolClaims), then true.
//
// Rationale: tool-using sessions rely on the model encoding each change as a
// tool call the client can execute. Coding models occasionally instead reply
// with a prose "done" statement (e.g. "已修正…/已更新文件…" or "fixed …
// updated …") without emitting any call — often after misreading a casual
// instruction such as "不用进行测试" ("don't run tests") as permission to skip
// the change entirely. Forwarding that prose as the final answer makes the
// client believe the file was changed when it was not.
func unfulfilledRepairEnabled() bool {
	if raw, ok := os.LookupEnv("M365_REPAIR_UNFULFILLED_TOOL_CLAIMS"); ok && strings.TrimSpace(raw) != "" {
		return envBoolDefault("M365_REPAIR_UNFULFILLED_TOOL_CLAIMS", true)
	}
	return currentSettings().RepairUnfulfilledToolClaims
}

// completionClaimPatterns are completed-change assertions (past tense /
// 已…/…了) that, combined with a file/code entity reference, signal the model
// believes it finished a modification in this round.
var completionClaimPatterns = []string{
	"已修正", "已修改", "已更新", "已改写", "已重写", "已写入", "已保存", "已创建", "已生成",
	"已替换", "已删除", "已调整", "已恢复", "已重构", "已添加", "已补充", "已完成",
	"修好了", "改好了", "更新了", "修改了", "修正了", "修复了", "写入了", "保存了",
	"生成了", "创建了", "替换了", "删除了", "重写了", "重新生成", "改成了",
	"fixed", "updated", "modified", "rewrote", "rewritten", "wrote", "saved",
	"created", "generated", "completed", "replaced", "deleted", "adjusted",
	"repaired", "patched", "refactored",
}

// fileEntityPatterns anchor a completion claim to an actual file/code target so
// plain status chatter ("完成更新了", "all done") does not trigger the repair.
var fileEntityPatterns = []string{
	"文件", "源码", "源代码", "代码", "脚本", "配置", "文档", "页面",
	`\.[A-Za-z0-9]{1,5}\b`,      // file extensions: .html .go .py ...
	`[A-Za-z]:[\\/]`,            // Windows absolute paths
	`(?:^|\s)/[A-Za-z0-9_./-]+`, // POSIX-style paths
	"file", "script", "source", "source code",
}

var (
	completionClaimRe = makePattern(completionClaimPatterns)
	fileEntityRe      = makePattern(fileEntityPatterns)
)

func makePattern(terms []string) *regexp.Regexp {
	return regexp.MustCompile(`(` + strings.Join(terms, "|") + `)`)
}

// unfulfilledToolClaimed reports whether an assistant answer asserts that a
// file/code change was completed ("已修正…/已更新文件…", "fixed the … in …")
// even though the current round produced no tool call. The check is
// deliberately narrow — both a completion assertion and a file/code entity
// must be present — so ordinary prose answers and future-tense plans never
// enter the repair path. English patterns match case-insensitively; Chinese
// is unaffected by lowercasing.
func unfulfilledToolClaimed(text string) bool {
	if len(text) > 8192 {
		text = text[:8192]
	}
	low := strings.ToLower(text)
	if !completionClaimRe.MatchString(low) {
		return false
	}
	return fileEntityRe.MatchString(low)
}

// unfulfilledClaimRepairText builds the one-shot repair prompt for a round
// whose answer claimed a completed change without any tool call. It re-opens
// the exact instructions from the existing tool encodings (JSON calls
// envelope), corrects the common "no tests == no work" misreading, and gives
// the model an honest escape hatch so a repair that yields no calls falls back
// to the original text instead of failing the request.
func unfulfilledClaimRepairText(toolMaps []map[string]any, prompt string) string {
	defs, _ := json.Marshal(toolMaps)
	return fmt.Sprintf(`Your previous reply claimed a completed change ("已修正…/已更新文件…", "fixed …/updated …") but the client received NO tool call, so nothing was actually modified. A prose claim without a tool call does not change any file.

Note: if the user said "不用进行测试" ("no tests needed"), that means testing is not required after the change — it does NOT mean the change itself can be skipped.

Re-do the work now:
1. If the file change is still needed, output ONLY a JSON tool call envelope that makes the change real:
   {"calls":[{"name":"function_name","arguments":{}}]}
   - read the target file first if you need its exact current content
   - for edit calls, copy old_string EXACTLY from the most recent read output; old_string and new_string MUST differ
   - validate every argument against FUNCTION_DEFINITIONS
2. If no tool call is actually required to satisfy the request, output exactly:
   NO_TOOL_NEEDED: no file or code change is being performed

NEVER claim a file was changed unless a tool call in this envelope performs it.

Application context and evidence:
%s

FUNCTION_DEFINITIONS:
%s`, prompt, string(defs))
}
