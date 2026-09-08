package chathub

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunesShortPassesThrough(t *testing.T) {
	s := "hello"
	if got := truncateRunes(s, 10); got != s {
		t.Fatalf("got %q", got)
	}
}

func TestTruncateRunesDoesNotSplitRune(t *testing.T) {
	// 10 bytes of ASCII + one 3-byte rune: limit 11 must cut before the rune,
	// not mid-rune.
	s := "aaaaaaaaaa中"
	got := truncateRunes(s, 11)
	if got != "aaaaaaaaaa" {
		t.Fatalf("got %q, want clean ASCII prefix", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("result is not valid UTF-8")
	}
}

func TestTruncateRunesKeepsWholeRuneWhenItFits(t *testing.T) {
	s := "中中中"
	if got := truncateRunes(s, 9); got != s {
		t.Fatalf("got %q, want unchanged", got)
	}
	if got := truncateRunes(s, 8); got != "中中" {
		t.Fatalf("got %q, want two runes", got)
	}
}

func TestTruncateRunesLargeString(t *testing.T) {
	s := strings.Repeat("中", 1000)
	got := truncateRunes(s, 500)
	if !utf8.ValidString(got) {
		t.Fatal("result is not valid UTF-8")
	}
	if len(got) != 498 { // 166 whole runes
		t.Fatalf("got %d bytes, want 498", len(got))
	}
}
