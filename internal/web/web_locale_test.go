package web

import (
	"os"
	"strings"
	"testing"
)

func TestWebIndexDefaultsToChineseUntilLocaleIsSelected(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{
		"const localeSelectionKey='m365_locale_selected';",
		"function preferredLocale()",
		"return 'zh-CN';",
		"localStorage.setItem(localeSelectionKey,'1')",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("web index missing Chinese default bootstrap %q", needle)
		}
	}
}

func TestWebIndexLocalizesTimeZoneOptions(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{
		"const timeZoneNames={",
		"function renderTimeZoneOptions()",
		"renderTimeZoneOptions();",
		"'zh-CN':'北京时间'",
		"'ja':'日本標準時'",
		"'ko':'싱가포르 표준시'",
		"'es':'Hora de Nueva York'",
		"'fr':'Heure de Londres'",
		"'de':'Berliner Zeit'",
		"'pt-BR':'Horário de Los Angeles'",
		"'ru':'Китайское стандартное время'",
		"'ar':'توقيت لندن'",
		"selector.value=selectedValue;",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("web index missing localized time-zone support %q", needle)
		}
	}
}

func TestWebIndexIncludesAccountMonitoringControls(t *testing.T) {
	body, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, needle := range []string{
		`data-f="cooldown"`,
		`x.status==='cooldown'`,
		`/api/accounts/schedule`,
		`x.callCount||0`,
		`x.rateLimited`,
		`if(x.rateLimited)callsTd.title=translateText('Account is currently rate limited');`,
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("web index missing cooldown control %q", needle)
		}
	}
}
