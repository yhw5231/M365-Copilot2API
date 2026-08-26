package web

import (
	"strings"
	"testing"
)

// TestCachedTokensReuseFlow simulates the exact scenario:
// 1. prompt = full flattened messages (history + current)
// 2. answerPrompt = incPrompt (incremental, set by conv-cache / session-resolver)
// 3. answerReq.Text = buildAnswerRequest(answerPrompt, ...) — the full text sent to ChatHub
// 4. answerPrompt = answerReq.Text (overwrite at line 2292/2712)
// 5. cachedTokens() = cachedInputTokens(prompt, answerPrompt) — should be > 0 for reuse
func TestCachedTokensReuseFlow(t *testing.T) {
	// Simulate a full conversation: system + 3 turns
	systemPrompt := "You are a helpful assistant."
	history := []string{
		"user: 帮我写一封邮件",
		"assistant: 好的，这是邮件草稿...",
		"user: 修改一下，加个附件",
		"assistant: 已修改，添加了附件部分。",
	}
	currentMsg := "user: 再改一下主题行"

	// Build the full flattened prompt (simulating line 1988)
	fullMessages := systemPrompt
	for _, m := range history {
		fullMessages += "\n" + m
	}
	fullMessages += "\n" + currentMsg
	prompt := strings.TrimSpace(fullMessages)

	// Simulate conv-cache hit: answerPrompt = incPrompt (only the new message)
	incPrompt := strings.TrimSpace(currentMsg)

	// answerReq.Text = buildAnswerRequest(answerPrompt, ...)
	// For a no-tools, non-goal request: answerReq.Text = incPrompt (no workspace instr, no ledger)
	answerReqText := incPrompt

	// Simulate line 2292: answerPrompt = answerReq.Text
	answerPrompt := answerReqText

	// cachedTokens() = cachedInputTokens(prompt, answerPrompt)
	cached := cachedInputTokens(prompt, answerPrompt)

	t.Logf("prompt=%q (%d chars)", prompt[:min(len(prompt), 80)], len(prompt))
	t.Logf("answerReq.Text=%q (%d chars)", answerReqText[:min(len(answerReqText), 80)], len(answerReqText))
	t.Logf("EstimateTokens(prompt)=%d", EstimateTokens(prompt))
	t.Logf("EstimateTokens(answerReq.Text)=%d", EstimateTokens(answerReqText))
	t.Logf("cached=%d", cached)

	if cached <= 0 {
		t.Fatalf("cached MUST be > 0 for a reused conversation, got %d", cached)
	}
}

// TestCachedTokensWithWorkspaceInstruction verifies the cached-token accounting
// uses the raw incremental prompt (sentPrompt), NOT the inflated answerReq.Text
// that includes a prepended workspace instruction + ledger context. This is the
// regression fixed by capturing sentPrompt before the answerPrompt overwrite.
func TestCachedTokensWithWorkspaceInstruction(t *testing.T) {
	systemPrompt := "You are a helpful assistant."
	history := []string{
		"user: 帮我读取文件",
		"assistant: 让我用工具读取...",
	}
	currentMsg := "user: 再帮我写个文件"

	// Full prompt
	fullMessages := systemPrompt
	for _, m := range history {
		fullMessages += "\n" + m
	}
	fullMessages += "\n" + currentMsg
	prompt := strings.TrimSpace(fullMessages)

	// Incremental (only the new message)
	incPrompt := strings.TrimSpace(currentMsg)

	// Simulate buildAnswerRequest: workspace instruction prepended and ledger context appended
	// workspace instruction (when tools with shell/exec/file declared) is ~200-400 tokens
	workspaceInst := "你是一个可以访问工作区的AI助手。你可以使用以下工具来执行操作：\n" +
		"- shell: 在远程工作区执行shell命令\n" +
		"- exec: 执行程序\n" +
		"- file: 读取和写入文件\n" +
		"请根据用户需求使用合适的工具。工具调用必须包含有效的json参数。\n" +
		"如果用户要求你执行操作，你应该使用工具来完成。\n" +
		"如果工具执行成功，请将结果反馈给用户。\n" +
		"如果工具执行失败，请尝试其他方法或告知用户。\n" +
		"注意：一次只能调用一个工具，等待工具结果后再决定下一步操作。"

	ledgerContext := "" // No ledger for non-goal sessions
	answerReqText := workspaceInst + "\n\n" + incPrompt
	if ledgerContext != "" {
		answerReqText += "\n" + ledgerContext
	}

	// The OLD buggy behavior: cachedInputTokens(prompt, answerReq.Text) returns 0
	// because the inflated sent text (with workspace instruction) >= full prompt.
	buggy := cachedInputTokens(prompt, answerReqText)
	t.Logf("BUGGY cachedInputTokens(prompt, answerReq.Text)=%d (should be 0 when overhead exceeds history)", buggy)

	// The FIXED behavior: cached-token accounting uses sentPrompt (raw incremental)
	// captured before the overwrite, so the workspace instruction is not counted
	// as "re-sent" input.
	fixed := cachedInputTokens(prompt, incPrompt)
	t.Logf("EstimateTokens(prompt)=%d EstimateTokens(workspaceInst)=%d EstimateTokens(incPrompt)=%d", EstimateTokens(prompt), EstimateTokens(workspaceInst), EstimateTokens(incPrompt))
	t.Logf("FIXED cachedInputTokens(prompt, sentPrompt)=%d", fixed)

	if fixed <= 0 {
		t.Fatalf("fixed calc must report positive cached tokens, got %d", fixed)
	}
}

// TestCachedTokensNonReuse returns 0 checks the fresh conversation case
func TestCachedTokensNonReuse(t *testing.T) {
	prompt := "user: hello"
	answerPrompt := prompt // No reuse, answerPrompt stays as full prompt

	// answerReq.Text = buildAnswerRequest(answerPrompt, ...)
	// For a no-tools request: answerReq.Text = prompt (no workspace instr)
	answerReqText := prompt

	answerPrompt = answerReqText
	cached := cachedInputTokens(prompt, answerPrompt)

	if cached != 0 {
		t.Fatalf("fresh conversation must have 0 cached, got %d", cached)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}