package web

import (
	"strings"
	"testing"
)

// The marker forms below are the exact sequences observed in real DeepSeek
// Harness sessions proxied through this gateway (provider m365xg): upstream
// ChatHub citation tokens referencing tool calls (fc_...) and turn sources.

func TestCitationMarkersStripCompleteNewFamily(t *testing.T) {
	text := "答案是 42。" + "\ue200cite\ue202fc_abc\ue201" + " 更多内容"
	if got := sanitizeCitationMarkers(text); got != "答案是 42。 更多内容" {
		t.Fatalf("complete marker was not stripped: %q", got)
	}
}

func TestCitationMarkersStripMultipleIds(t *testing.T) {
	text := "均不存在 Unicode 替换字符。" + "\ue200cite\ue202fc_bee6e1ca-932e-4aba-9502-8f0674897f3b\ue202fc_c70877de-497b-4bb7-83d9-3540689bbebc\ue201" + "\n\n结束"
	if got := sanitizeCitationMarkers(text); got != "均不存在 Unicode 替换字符。\n\n结束" {
		t.Fatalf("multi-id marker was not stripped: %q", got)
	}
}

func TestCitationMarkersStripDanglingFragment(t *testing.T) {
	// A truncated stream left the trailing marker open and cut mid-uuid.
	text := "目标状态已更新为 **complete**。" + "\ue200cite\ue202fc_33356c1c-8325-477a-a5ff-5"
	if got := sanitizeCitationMarkers(text); got != "目标状态已更新为 **complete**。" {
		t.Fatalf("dangling marker fragment was not stripped: %q", got)
	}
}

func TestCitationMarkersStripCompleteFollowedByDangling(t *testing.T) {
	text := "字符。" + "\ue200cite\ue202fc_a\ue201" + " 目标状态已更新为 **complete**。" + "\ue200cite\ue202fc_33356c1c-8325-477a-a5ff"
	if got := sanitizeCitationMarkers(text); got != "字符。 目标状态已更新为 **complete**。" {
		t.Fatalf("complete+dangling mix not cleaned: %q", got)
	}
}

func TestCitationMarkersStripLegacyFamily(t *testing.T) {
	text := "答案。" + "\uE008" + "cite" + "\uE009turn1search2\uE009turn3news4\uE009" + " 更多"
	if got := sanitizeCitationMarkers(text); got != "答案。 更多" {
		t.Fatalf("legacy family marker was not stripped: %q", got)
	}
	if got := sanitizeCitationMarkers("答案。" + "\uE008" + "cite" + "\uE009turn1search2"); got != "答案。" {
		t.Fatalf("legacy dangling marker was not stripped: %q", got)
	}
}

func TestCitationMarkersStripXMLForm(t *testing.T) {
	if got := sanitizeCitationMarkers("答案是 42。<cite>turn4search6, turn1search2</cite> 更多"); got != "答案是 42。 更多" {
		t.Fatalf("xml citation was not stripped: %q", got)
	}
}

func TestCitationMarkersPreservePlainText(t *testing.T) {
	text := "普通答案没有任何标记。\n第二行。"
	if got := sanitizeCitationMarkers(text); got != text {
		t.Fatalf("plain text was changed: %q", got)
	}
	// A lone PUA char with no "cite" token is not a citation marker.
	lone := "答案" + "\ue200" + "内容"
	if got := sanitizeCitationMarkers(lone); got != lone {
		t.Fatalf("lone control char was removed: %q", got)
	}
}

func TestCitationMarkersKeepMode(t *testing.T) {
	t.Setenv("M365_KEEP_CITE_MARKERS", "1")
	text := "答案。" + "\ue200cite\ue202fc_a\ue201" + " 更多"
	if got := sanitizeCitationMarkers(text); got != text {
		t.Fatalf("markers were stripped in keep mode: %q", got)
	}
}

func TestCitationStreamFilterMarkerSplitAcrossDeltas(t *testing.T) {
	f := newCitationStreamFilter()
	if out := f.Push("答案是 42。" + "\ue200cite\ue202fc_"); out != "答案是 42。" {
		t.Fatalf("text before the open marker was not released: %q", out)
	}
	if out := f.Push("1\ue201 继续。"); out != " 继续。" {
		t.Fatalf("closed marker not cleaned after close: %q", out)
	}
	if out := f.Flush(); out != "" {
		t.Fatalf("flush produced trailing text: %q", out)
	}
}

func TestCitationStreamFilterDanglingAtStreamEnd(t *testing.T) {
	// Tools-mode stream: the whole answer arrives in one emit, then flush.
	text := "并验证配置控件、用户管理文案及静态资源中均不存在 Unicode 替换字符。" + "\ue200cite\ue202fc_bee6e1ca-932e-4aba-9502-8f0674897f3b\ue202fc_c70877de-497b-4bb7-83d9-3540689bbebc\ue201" + "\n\n目标状态已更新为 **complete**。" + "\ue200cite\ue202fc_33356c1c-8325-477a-a5ff-5"
	f := newCitationStreamFilter()
	out := f.Push(text)
	out += f.Flush()
	want := "并验证配置控件、用户管理文案及静态资源中均不存在 Unicode 替换字符。\n\n目标状态已更新为 **complete**。"
	if out != want {
		t.Fatalf("session tail not cleaned:\ngot:  %q\nwant: %q", out, want)
	}
}

func TestCitationStreamFilterImmediatePassthrough(t *testing.T) {
	f := newCitationStreamFilter()
	if out := f.Push("没有标记的文本"); out != "没有标记的文本" {
		t.Fatalf("plain delta was held back: %q", out)
	}
	if out := f.Flush(); out != "" {
		t.Fatalf("flush produced text: %q", out)
	}
}

func TestCitationStreamFilterCompleteMarkerSingleDelta(t *testing.T) {
	f := newCitationStreamFilter()
	if out := f.Push("登录验证成功。" + "\ue200cite\ue202fc_68167620-2861-4766-9577-a8e2c603aedc\ue201" + "\n\n首次登录请修改密码。"); out != "登录验证成功。\n\n首次登录请修改密码。" {
		t.Fatalf("complete marker not stripped inline: %q", out)
	}
	if out := f.Flush(); out != "" {
		t.Fatalf("flush produced text: %q", out)
	}
}

func TestCitationStreamFilterRespectsKeepMode(t *testing.T) {
	t.Setenv("M365_KEEP_CITE_MARKERS", "1")
	f := newCitationStreamFilter()
	text := "答案。" + "\ue200cite\ue202fc_a\ue201" + " 更多"
	out := f.Push("前")
	out += f.Push("缀" + text)
	out += f.Flush()
	if out != "前缀" + text {
		t.Fatalf("keep-mode stream filter changed text: %q", out)
	}
}

func TestCitationSanitizerMatchesObservedSession(t *testing.T) {
	if strings.Contains(sanitizeCitationMarkers("目标状态已更新为 **complete**。"+"\ue200cite\ue202fc_33356c1c-8325-477a-a5ff-5ee9c4cddde5\ue201"), "\ue200") {
		t.Fatal("closed marker survived sanitize")
	}
}

// File-marker pairs are stripped while the path text between the tags is kept,
// mirroring how the upstream UI renders them as file chips.

func TestFileMarkersStripPairKeepPath(t *testing.T) {
	text := "审查对象：<File>src/recruitment_agent/main.py</File>，重点覆盖安全。"
	if got := sanitizeUpstreamMarkers(text); got != "审查对象：src/recruitment_agent/main.py，重点覆盖安全。" {
		t.Fatalf("file pair not stripped cleanly: %q", got)
	}
}

func TestFileMarkersStripMultiplePairs(t *testing.T) {
	text := `<File>README.md</File>、<File>docs\architecture.md</File> 和 <File>docs\status-and-limitations.md</File> 需要校正。`
	want := `README.md、docs\architecture.md 和 docs\status-and-limitations.md 需要校正。`
	if got := sanitizeUpstreamMarkers(text); got != want {
		t.Fatalf("multiple pairs not stripped cleanly:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFileMarkersStripWindowsAbsolutePath(t *testing.T) {
	text := `- <File>D:\NET\ai\ythh-1\src\recruitment_agent\api\app.py</File> 中` + "`create_app()` 创建 ASGI 应用。"
	want := `- D:\NET\ai\ythh-1\src\recruitment_agent\api\app.py 中` + "`create_app()` 创建 ASGI 应用。"
	if got := sanitizeUpstreamMarkers(text); got != want {
		t.Fatalf("absolute-path pair not stripped:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFileMarkersCaseInsensitive(t *testing.T) {
	if got := sanitizeUpstreamMarkers("<file>路径</FILE> 内容"); got != "路径 内容" {
		t.Fatalf("case-insensitive pair not handled: %q", got)
	}
}

func TestFileMarkersUnclosedAndOrphanTags(t *testing.T) {
	// Complete open tag with no close: tag removed, following text kept.
	if got := sanitizeUpstreamMarkers("见 <File>README.md 的说明。"); got != "见 README.md 的说明。" {
		t.Fatalf("unclosed open tag not handled: %q", got)
	}
	// Orphan close tag: removed.
	if got := sanitizeUpstreamMarkers("见 README.md</File> 的说明。"); got != "见 README.md 的说明。" {
		t.Fatalf("orphan close tag not handled: %q", got)
	}
	// Damaged tag (no ">"): token removed, text kept.
	if got := sanitizeUpstreamMarkers("见 <FileREADME.md 的说明。"); got != "见 README.md 的说明。" {
		t.Fatalf("damaged open tag not handled: %q", got)
	}
}

func TestFileMarkersTruncatedPartialAtEnd(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"已读取 <File", "已读取 "},
		{"已读取 <Fi", "已读取 "},
		{"已读取 <Fil", "已读取 "},
		{"已读取 </Fil", "已读取 "},
		{"已读取 </F", "已读取 "},
	} {
		if got := sanitizeUpstreamMarkers(tc.in); got != tc.want {
			t.Fatalf("truncated partial %q not cleaned: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A truncated stream can also drop the marker close mid-answer; the unclosed
// token is protocol-shaped and is stripped even without a close.

func TestCitationMarkersStripMidTextUnclosed(t *testing.T) {
	text := "答案。" + "\ue200cite\ue202fc_9c79be70-f42c-49f2-ab3e-8a8f37c4c295" + " 内容继续。"
	if got := sanitizeCitationMarkers(text); got != "答案。 内容继续。" {
		t.Fatalf("mid-text unclosed marker not stripped: %q", got)
	}
	text = "答案。" + "\ue200cite\ue202turn3search4" + " 内容继续。"
	if got := sanitizeCitationMarkers(text); got != "答案。 内容继续。" {
		t.Fatalf("mid-text unclosed turn marker not stripped: %q", got)
	}
	text = "答案。" + "\uE008" + "cite" + "\uE009fc_9c79be70-f42c-49f2-ab3e-8a8f37c4c295" + " 内容继续。"
	if got := sanitizeCitationMarkers(text); got != "答案。 内容继续。" {
		t.Fatalf("mid-text unclosed legacy marker not stripped: %q", got)
	}
	// Non-protocol text after the open must not be eaten without a close.
	text = "答案。" + "\ue200cite\ue202我们在这里" + " 内容继续。"
	if got := sanitizeCitationMarkers(text); got != text {
		t.Fatalf("non-protocol content was stripped: %q", got)
	}
}

// The stream filter must hold a <File> tag split across deltas (even mid-tag)
// until it is complete or the stream ends.

func TestCitationStreamFilterFileTagSplitAcrossDeltas(t *testing.T) {
	f := newCitationStreamFilter()
	if out := f.Push("审查对象：<File>READ"); out != "审查对象：" {
		t.Fatalf("text before the partial tag was not released: %q", out)
	}
	if out := f.Push("ME.md</File> 重点覆盖"); out != "README.md 重点覆盖" {
		t.Fatalf("completed file tag not cleaned after close: %q", out)
	}
	if out := f.Flush(); out != "" {
		t.Fatalf("flush produced trailing text: %q", out)
	}
}

func TestCitationStreamFilterFileTagSplitMidTag(t *testing.T) {
	f := newCitationStreamFilter()
	if out := f.Push("见 <Fil"); out != "见 " {
		t.Fatalf("partial open tag leaked: %q", out)
	}
	if out := f.Push("e>src/main.py</File> 说明"); out != "src/main.py 说明" {
		t.Fatalf("completed split tag not cleaned: %q", out)
	}
	if out := f.Flush(); out != "" {
		t.Fatalf("flush produced trailing text: %q", out)
	}
}

func TestCitationStreamFilterFileTagHeldUntilFlush(t *testing.T) {
	f := newCitationStreamFilter()
	if out := f.Push("并加载 <File>D:\\NET\\ai\\ythh-1\\src"); out != "并加载 " {
		t.Fatalf("unclosed file tag was released: %q", out)
	}
	if out := f.Flush(); out != "D:\\NET\\ai\\ythh-1\\src" {
		t.Fatalf("dangling <File> tail not stripped at flush: %q", out)
	}
}

func TestCitationStreamFilterMixedMarkers(t *testing.T) {
	f := newCitationStreamFilter()
	text := "结果见 <File>src/a.py</File>。" + "\ue200cite\ue202fc_abc\ue201" + " 完毕"
	if out := f.Push(text); out != "结果见 src/a.py。 完毕" {
		t.Fatalf("mixed markers not cleaned: %q", out)
	}
}

// Marker stripping must not depend on the opt-in identity policy: the
// non-stream delivery paths call sanitizePublic* directly, and a fresh
// deployment without M365_PUBLIC_IDENTITY_POLICY still must not leak the raw
// channel protocol to the client.

func TestUpstreamMarkersStrippedWithoutIdentityPolicy(t *testing.T) {
	text := "答案。" + "\ue200cite\ue202fc_abc\ue201" + " 见 <File>src/main.py</File>。"
	if got := sanitizePublicAssistantTextForModel(text, "m365xg"); got != "答案。 见 src/main.py。" {
		t.Fatalf("markers leaked without identity policy: %q", got)
	}
	if got := sanitizePublicReasoningText("推理。" + "\ue200cite\ue202fc_abc\ue201" + " 继续"); got != "推理。 继续" {
		t.Fatalf("markers leaked in reasoning without identity policy: %q", got)
	}
}

func TestUpstreamMarkersKeptInKeepMode(t *testing.T) {
	t.Setenv("M365_KEEP_CITE_MARKERS", "1")
	text := "答案 <File>src/main.py</File>。" + "\ue200cite\ue202fc_a\ue201"
	if got := sanitizeUpstreamMarkers(text); got != text {
		t.Fatalf("file/cite markers were stripped in keep mode: %q", got)
	}
}