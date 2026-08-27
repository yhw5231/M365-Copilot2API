package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

type detectedToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func toolType(name string, tools []map[string]any) string {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			if typ, _ := t["type"].(string); typ != "" {
				return typ
			}
		}
	}
	return "function"
}

func allowedToolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok && n != "" {
				out[n] = true
			}
		}
	}
	return out
}

type rejectedToolCall struct {
	Name   string
	Reason string
}

// validateDetectedToolCalls is the final trust boundary before a model-selected
// call is serialized to the client. ChatHub/native events and model-generated
// routing text are both untrusted: an undeclared name such as "unknown_tool"
// must never escape to Claude Code, Codex, or another local tool runner.
func validateDetectedToolCalls(calls []detectedToolCall, tools []map[string]any, choice any) ([]detectedToolCall, []rejectedToolCall) {
	valid := make([]detectedToolCall, 0, len(calls))
	rejected := make([]rejectedToolCall, 0)
	for _, call := range calls {
		fn := toolFunction(call.Name, tools)
		if fn == nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool was not declared by the client"})
			continue
		}
		if !toolChoiceAllows(choice, call.Name) {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool_choice does not allow this tool"})
			continue
		}
		args := map[string]any{}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			call.Arguments = json.RawMessage(`{}`)
		} else if err := json.Unmarshal(call.Arguments, &args); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "arguments are not a JSON object"})
			continue
		}
		if err := schemaValid(args, fn); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: err.Error()})
			continue
		}
		if call.ID == "" {
			call.ID = callID(call.Name, string(call.Arguments), len(valid))
		}
		if call.Type == "" {
			call.Type = toolType(call.Name, tools)
		}
		valid = append(valid, call)
	}
	return valid, rejected
}

func toolChoiceAllows(choice any, name string) bool {
	if choice == nil {
		return true
	}
	if s, ok := choice.(string); ok {
		return s != "none" && (s != "required" || name != "")
	}
	if m, ok := choice.(map[string]any); ok {
		if f, ok := m["function"].(map[string]any); ok {
			n, _ := f["name"].(string)
			return n == name
		}
		if n, ok := m["name"].(string); ok {
			return n == name
		}
	}
	return true
}

// callID returns a globally unique tool call id. Content hashes previously
// collided when the same tool+arguments was invoked again (duplicate tool call
// id errors from clients), so uniqueness must not depend on call content.
func callID(name, args string, index int) string {
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func extractToolCalls(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	allowed := allowedToolNames(tools)
	var out []detectedToolCall
	remaining := text
	for {
		start := strings.Index(remaining, "<m365-tool-call>")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], "</m365-tool-call>")
		if end < 0 {
			break
		}
		end += start
		content := remaining[start+len("<m365-tool-call>") : end]
		remaining = remaining[end+len("</m365-tool-call>"):]
		var raw any
		if json.Unmarshal([]byte(content), &raw) != nil {
			continue
		}
		items := []any{raw}
		if arr, ok := raw.([]any); ok {
			items = arr
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			n, _ := m["name"].(string)
			if !allowed[n] || !toolChoiceAllows(choice, n) {
				continue
			}
			a, _ := json.Marshal(m["arguments"])
			out = append(out, detectedToolCall{ID: callID(n, string(a), len(out)), Type: toolType(n, tools), Name: n, Arguments: a})
		}
	}
	return out, len(out) > 0
}

func validateToolResult(messages []oaiMsg, known map[string]bool) error {
	for _, m := range messages {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required")
			}
			if len(known) > 0 && !known[m.ToolCallID] {
				return fmt.Errorf("unknown tool_call_id: %s", m.ToolCallID)
			}
		}
	}
	return nil
}

var toolRefusalPatterns = []string{
	"tools are not available",
	"tool is not available",
	"not actually registered",
	"not actually available",
	"not available in this session",
	"工具不可用",
	"工具未暴露",
}

func isToolRefusal(text string) bool {
	if len(text) >= 200 {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range toolRefusalPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

var contentPolicyPatterns = []string{
	"很抱歉，我无法响应",
	"我很抱歉，我无法响应",
	"i'm sorry, i can't respond",
	"i'm sorry, i cannot respond",
}

func isContentPolicyBlock(text string) bool {
	if len(text) > 300 {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range contentPolicyPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// workspaceToolMisjudgmentPatterns are exact, high-precision phrases that
// indicate the model wrongly claims the session lacks caller-provided tools or
// workspace access. Exact matches remain the fast, zero-false-positive path,
// but they are intentionally NOT the only detector: models rephrase denials
// freely, so isWorkspaceToolMisjudgment layers structural and tool-aware
// signals on top of this list (see below).
var workspaceToolMisjudgmentPatterns = []string{
	"cannot access the windows path because this session only provides linux",
	"cannot access your windows workspace because this session only provides linux",
	"this session only provides a linux container",
	"this session only has a linux sandbox",
	"the current session only has access to /mnt/data",
	"i can only access /mnt/data",
	"i don't have any tools in this session",
	"no tools are available in this session",
	"the provided tools are not available in this session",
	"i don't have command execution in this session",
	"i cannot access the actual workspace",
	"i cannot access your actual workspace",
	"当前会话只提供 linux 容器",
	"当前会话只能访问 /mnt/data",
	"当前会话没有任何可用工具",
	"当前会话没有执行工具",
	"无法访问实际工作区",
	"无法访问你的实际工作区",
}

// workspaceToolAvailabilityDenials are compound availability claims (EN+ZH).
// Bare "没有"/"无法"/"不能" are deliberately absent: they appear in perfectly
// legitimate statements ("当前会话没有使用文件操作工具", "无法完成任务") and
// would make the generic detector over-match.
var workspaceToolAvailabilityDenials = []string{
	// English
	"don't have", "do not have", "doesn't have", "does not have",
	"doesn't provide", "does not provide", "not available", "not provided",
	"not installed", "not configured", "not registered", "not callable",
	"not accessible", "unavailable", "no access to", "no tools", "no tool",
	"without", "lacks", "lacking", "missing", "absent", "none available",
	"not exist", "doesn't exist", "does not exist", "not present",
	// Chinese
	"没有可调用", "没有可用的", "没有提供", "不提供", "没有任何工具", "没有工具",
	"无法调用", "不能调用", "不可用", "未提供", "不具备", "不存在",
	"缺少", "找不到", "没有执行", "没有命令", "没有接口", "没有安装",
}

// workspaceToolChannelNouns are unambiguous execution/tool-channel nouns (EN+ZH).
// English file-tool names (read/write/edit/glob/grep) are intentionally absent:
// they are ordinary English words, so a generic layer using them would
// over-match ("I don't have time to write the report"). Bare file-tool names
// are only meaningful in the tool-aware layer, which knows the caller actually
// declared them.
var workspaceToolChannelNouns = []string{
	"tool", "tools", "tooling", "shell", "execution", "command", "commands",
	"interface", "interfaces", "pwsh", "powershell", "bash", "exec",
	"file operation", "file tool",
	// Chinese
	"工具", "接口", "可调用", "执行", "命令", "文件操作", "shell", "pwsh",
	"powershell", "bash",
}

// workspaceToolScopeNouns are session/workspace/environment references that
// anchor an availability claim to the current session.
var workspaceToolScopeNouns = []string{
	"session", "workspace", "environment", "current session", "this session",
	"the session",
	// Chinese
	"当前会话", "本会话", "本次会话", "当前环境", "当前工作区", "工作区", "本机",
}

// workspaceToolMisjudgmentRegexes capture common structural shapes of a
// workspace/tool-availability misjudgment without requiring the exact wording.
// Each pattern combines a denial with a channel noun and/or session scope, so
// ordinary technical discussion (a failed command, a locked file, a sandbox
// deployment note) does not match.
var workspaceToolMisjudgmentRegexes = []*regexp.Regexp{
	// EN: denial ... channel noun ... session scope
	regexp.MustCompile(`(?i)(don'?t have|do not have|doesn'?t provide|does not provide|no (longer )?(tools?|shell|execution|command|interface|access to)|not (available|provided|installed|configured|registered|callable|accessible)|unavailable|lacks?|missing|absent|without) .{0,50}(tools?|shell|execution|command|interface|pwsh|powershell|bash|exec|file (operation|tool)) .{0,60}(in this|in the|this|the|current) (session|environment|workspace)`),
	// EN: session scope first (bare "no" is excluded — it is too wide and
	// produces false positives on "this session ... no ... shell was background"
	// phrases; the semantic co-occurrence layer and EN1 still catch "no tools"
	// and "no shell" through the "no (longer )?(tools?|shell|...)" branch).
	regexp.MustCompile(`(?i)(this|the|current) (session|environment|workspace) .{0,60}(don'?t have|do not have|doesn'?t provide|does not provide|without|lacks?|not (available|provided)|unavailable|missing|absent) .{0,50}(tools?|shell|execution|command|interface|pwsh|powershell|bash|exec)`),
	// ZH: 当前会话 ... 没有可调用/没有提供 ... 工具/接口
	regexp.MustCompile(`(当前会话|本会话|本次会话|当前环境|当前工作区).{0,40}(没有可调用|没有可用的|没有提供|不提供|没有任何工具|没有工具|无法调用|不能调用|不可用|未提供|不具备|不存在|缺少|找不到|没有执行|没有命令|没有接口).{0,40}(工具|接口|可调用|文件操作|执行|命令|pwsh|powershell|bash|shell)`),
	// ZH: 没有可调用/没有可用 ... 工具/接口 (no session word needed)
	regexp.MustCompile(`(没有可调用|没有可用的|没有提供|不提供|没有任何工具|无法调用|不能调用|不可用|未提供|不具备|不存在|缺少|找不到|没有执行|没有命令|没有接口).{0,40}(工具|接口|文件操作|执行|命令|pwsh|powershell|bash|shell)`),
	// ZH: <工具/接口> 不存在/不可用/未提供
	regexp.MustCompile(`(工具|接口|pwsh|powershell|bash|shell|文件操作).{0,20}(不存在|不可用|未提供|没有提供|无法调用|不能调用)`),
	// EN/ZH: environment exclusivity (linux container / sandbox / /mnt/data)
	regexp.MustCompile(`(?i)(this |the |current )?(session|environment|workspace|sandbox|本机|当前)( |)(only|can only|only has|only provides|只能|仅能|只提供|只允许) .{0,40}(linux|container|sandbox|/mnt/data|cloud|容器|沙箱)`),
}

// containsAnyTerm reports whether s contains any of the given terms.
func containsAnyTerm(s string, terms []string) bool {
	for _, t := range terms {
		if t != "" && strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func isSandboxHallucination(text string) bool {
	return isWorkspaceToolMisjudgment(text)
}

// workspaceToolMisjudgmentScanLimit bounds the low-precision detection layers
// (structural regexes, semantic co-occurrence, tool-aware) to the opening of an
// assistant reply. Misjudgment claims almost always appear at the very start of
// a reply ("目前仍无法继续修改：当前会话实际没有可调用的..."), because the
// model refuses to proceed before doing anything else. Legitimate mid-reply
// technical narration (permission errors, sandbox notes, command failures) can
// contain the same denial/channel/scope vocabulary, so scanning only the
// opening keeps recall for the real failure mode while cutting false positives
// from long replies. The count is in bytes: 150 bytes ≈ 50 汉字 (UTF-8 Chinese
// is 3 bytes per character), which covers the opening claim of a misjudgment
// while keeping mid-reply narration out of the scan window. The exact-phrase
// layer still scans the whole text: those phrases are unambiguous misjudgments
// and cannot produce false positives.
//
// The value is a variable (not a const) so the accuracy/speed trade-off across
// window sizes can be measured by tests; production code never mutates it.
var workspaceToolMisjudgmentScanLimit = 150

// workspaceToolMisjudgmentScanText returns the opening window of text used by
// the structural/semantic/tool-aware layers. The whole text is returned when it
// fits within the scan limit. The cut is moved back to a UTF-8 rune boundary so
// a truncated multibyte character can never distort a pattern match.
func workspaceToolMisjudgmentScanText(low string) string {
	if len(low) <= workspaceToolMisjudgmentScanLimit {
		return low
	}
	end := workspaceToolMisjudgmentScanLimit
	for end > 0 && low[end]&0xC0 == 0x80 {
		end--
	}
	return low[:end]
}

// isWorkspaceToolMisjudgment identifies explicit, incorrect claims that the
// current gateway or session can access only a different environment, lacks
// caller-provided tools, or cannot access the actual workspace. Ordinary
// technical discussion of containers, sandboxes, /mnt/data, code interpreters,
// or legitimate tool failures is intentionally not classified as pollution.
//
// Detection is layered so reworded denials are still caught:
//  1. exact phrases (workspaceToolMisjudgmentPatterns) — fast, zero false
//     positives, scanned over the whole text
//  2. structural regexes (workspaceToolMisjudgmentRegexes) — common rephrasings
//  3. semantic co-occurrence — availability denial + channel noun + session scope
//
// Layers 2 and 3 only examine the opening window
// (workspaceToolMisjudgmentScanLimit), because misjudgments are opening claims
// while legitimate denial-shaped narration usually appears later in a reply.
func isWorkspaceToolMisjudgment(text string) bool {
	low := strings.ToLower(strings.TrimSpace(text))
	if low == "" {
		return false
	}
	for _, pattern := range workspaceToolMisjudgmentPatterns {
		if strings.Contains(low, strings.ToLower(pattern)) {
			return true
		}
	}
	scan := workspaceToolMisjudgmentScanText(low)
	for _, re := range workspaceToolMisjudgmentRegexes {
		if re.MatchString(scan) {
			return true
		}
	}
	return containsAnyTerm(scan, workspaceToolAvailabilityDenials) &&
		containsAnyTerm(scan, workspaceToolChannelNouns) &&
		containsAnyTerm(scan, workspaceToolScopeNouns)
}

// workspaceToolWeakDenials are broader denial terms used only by the
// tool-aware layer, where the text must additionally mention an actually
// declared tool name and a channel noun inside the window, and must not carry
// permission/access context.
var workspaceToolWeakDenials = []string{
	"don't have", "do not have", "doesn't have", "does not have",
	"没有", "无法", "不能", "不具备", "缺少", "找不到", "没有提供",
}

// workspaceToolPermissionContext is legitimate permission/access phrasing that
// must suppress a tool-aware match ("I don't have write access to that file").
var workspaceToolPermissionContext = []string{
	"access to", "permission", "permissions", "denied", "read-only", "locked",
	"not allowed", "unauthorized", "权限", "拒绝", "只读", "锁定", "不允许", "无权",
}

// isWorkspaceToolMisjudgmentForTools is the tool-aware variant used at call
// sites that know exactly which tools the caller declared. It is the most
// robust layer: regardless of how the model rephrases the denial, if it names
// one of the actually-declared tools and asserts that tool/interface is
// unavailable, the response is treated as a misjudgment. This catches phrasings
// no fixed list can anticipate (e.g. "当前会话实际没有可调用的 pwsh、read、
// write、edit、glob 或 grep 文件操作接口").
func isWorkspaceToolMisjudgmentForTools(text string, toolMaps []map[string]any) bool {
	if isWorkspaceToolMisjudgment(text) {
		return true
	}
	return toolAwareMisjudgment(text, toolMaps)
}

// toolAwareMisjudgment looks for an actually-declared tool name inside text,
// then checks the window around it for an availability denial. Word boundaries
// keep "read"/"write" from matching inside "already"/"rewrite", and
// permission/access phrasing ("write access to that file") suppresses the
// match so legitimate permission statements are not treated as pollution.
// Like the structural/semantic layers, it only scans the opening window of the
// reply: misjudgments are opening claims, while legitimate narration that
// merely names a tool ("the read tool returned an error") usually comes later.
func toolAwareMisjudgment(text string, toolMaps []map[string]any) bool {
	declared := extractToolNames(toolMaps)
	if len(declared) == 0 {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(text))
	if low == "" {
		return false
	}
	scan := workspaceToolMisjudgmentScanText(low)
	for name := range declared {
		lname := strings.ToLower(name)
		for _, idx := range wordIndexes(scan, lname) {
			start := idx - 80
			if start < 0 {
				start = 0
			}
			end := idx + len(lname) + 80
			if end > len(scan) {
				end = len(scan)
			}
			window := scan[start:end]
			if containsAnyTerm(window, workspaceToolPermissionContext) {
				continue
			}
			if containsAnyTerm(window, workspaceToolAvailabilityDenials) {
				return true
			}
			if containsAnyTerm(window, workspaceToolWeakDenials) &&
				containsAnyTerm(window, workspaceToolChannelNouns) {
				return true
			}
		}
	}
	return false
}

// wordIndexes returns the byte offsets of every whole-word occurrence of term
// in text. ASCII terms use \b boundaries (so "read" does not match inside
// "already"/"thread"); non-ASCII terms fall back to substring positions.
func wordIndexes(text, term string) []int {
	var out []int
	if term == "" {
		return out
	}
	if isASCIIWord(term) {
		re, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(term) + `\b`)
		if err == nil {
			for _, loc := range re.FindAllStringIndex(text, -1) {
				out = append(out, loc[0])
			}
			return out
		}
	}
	for i := 0; ; {
		j := strings.Index(text[i:], term)
		if j < 0 {
			break
		}
		out = append(out, i+j)
		i += j + len(term)
	}
	return out
}

func isASCIIWord(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// cleanWorkspaceToolMisjudgments removes only previously persisted assistant
// messages that contain a workspace/tool-availability misjudgment. User,
// system, developer, and tool messages are always preserved, including genuine
// discussions of /mnt/data, Linux containers, sandboxes, or Windows tooling.
// The caller's declared tools are used for the tool-aware layer; a nil or empty
// tool list falls back to the generic detector.
func cleanWorkspaceToolMisjudgments(messages []oaiMsg, toolMaps []map[string]any) []oaiMsg {
	if len(messages) == 0 {
		return nil
	}
	cleaned := make([]oaiMsg, 0, len(messages))
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			text := contentToString(msg.Content)
			if isWorkspaceToolMisjudgment(text) || toolAwareMisjudgment(text, toolMaps) {
				continue
			}
		}
		cleaned = append(cleaned, msg)
	}
	return cleaned
}

// workspaceEchoTerms are the environment-substitution vocabulary that, when the
// user's own question already contains them, makes the model's repetition of
// them a legitimate answer rather than a misjudgment (e.g. the user asks about
// "Linux容器 / /mnt/data / 无法访问工作区" and the model explains it).
var workspaceEchoTerms = []string{
	"linux", "container", "容器", "/mnt/data", "sandbox", "沙箱",
	"无法访问工作区", "只提供 linux", "只有 linux", "linux only",
}

// userPromptMentionsWorkspace reports whether the user's own prompt already
// contains the environment-substitution vocabulary. When it does, the model may
// legitimately repeat those terms while answering; the correction is only worth
// running when the user did not introduce them first.
func userPromptMentionsWorkspace(userPrompt string) bool {
	low := strings.ToLower(userPrompt)
	for _, term := range workspaceEchoTerms {
		if strings.Contains(low, term) {
			return true
		}
	}
	return false
}

// needsWorkspaceToolMisjudgmentCorrection applies the protocol-first gate before
// consulting the text detector. The correction only fires when:
//
//  1. the caller actually declared an execution or file tool — a caller with no
//     execution tool makes a "no shell" claim literally true, and there is no
//     local channel for the model to substitute away from;
//  2. no tool call has completed yet in the current turn — a model that already
//     executed a tool successfully cannot simultaneously claim the tools are
//     unavailable;
//  3. the user's own prompt does not already contain the workspace vocabulary —
//     answering a question about "Linux容器 / /mnt/data / 无法访问工作区" legitimately
//     repeats those terms, so the response is not a misjudgment.
//
// Only when all protocol gates pass does the layered text detector run, so
// ordinary conversation about containers or paths cannot trigger a correction.
func needsWorkspaceToolMisjudgmentCorrection(text string, toolMaps []map[string]any, userPrompt string, ledger agentLedger) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	// Protocol gate 1: the caller must have declared tools. Without a shell/exec
	// or file tool there is nothing to substitute and no misjudgment to repair.
	hasExec := hasDeclaredTools(toolMaps, append(append([]string{}, knownShellNames...), knownExecToolNames...))
	hasFiles := hasDeclaredTools(toolMaps, knownFileToolNames)
	if !hasExec && !hasFiles {
		return false
	}
	// Protocol gate 2: a model that already ran a tool this turn has proven the
	// tools exist; a later availability claim is not a substitution to correct.
	if len(ledger.Completed) > 0 {
		return false
	}
	// Protocol gate 3: echo suppression — if the user asked about the workspace
	// vocabulary first, the model repeating it is a legitimate answer.
	if userPromptMentionsWorkspace(userPrompt) {
		return false
	}
	return isWorkspaceToolMisjudgmentForTools(text, toolMaps)
}
