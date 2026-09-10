package chathub

import "testing"

func TestRateLimitNoticeText(t *testing.T) {
	noticed := []string{
		// 2026-09 上游实际返回的新文案（云服务器 trace 抓到的原文）。
		"我们目前流量较高。请稍后重试。",
		"当前流量较高，请稍后重试",
		"太多请求",
		"无法响应这么多请求",
		"Too many requests, please try again later.",
		"We're currently experiencing high traffic. Please try again soon.",
		"Please retry later",
	}
	for _, tc := range noticed {
		if !rateLimitNoticeText(tc) {
			t.Errorf("rateLimitNoticeText(%q) = false, want true", tc)
		}
	}

	answers := []string{
		"",
		"你好，我来帮你重构这段代码。",
		"以下是修复后的 diff：...",
		"Today's network traffic analysis shows normal patterns.",
	}
	for _, tc := range answers {
		if rateLimitNoticeText(tc) {
			t.Errorf("rateLimitNoticeText(%q) = true, want false", tc)
		}
	}
}
