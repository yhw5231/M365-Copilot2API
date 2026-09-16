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
	if strings.Contains(sanitizeCitationMarkers("目标状态已更新为 **complete**。" + "\ue200cite\ue202fc_33356c1c-8325-477a-a5ff-5ee9c4cddde5\ue201"), "\ue200") {
		t.Fatal("closed marker survived sanitize")
	}
}