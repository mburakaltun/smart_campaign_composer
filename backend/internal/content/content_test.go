package content

import (
	"strings"
	"testing"
)

func TestValidateLimits(t *testing.T) {
	ok := Messages{SMSText: strings.Repeat("a", 160), EmailSubject: strings.Repeat("b", 80), EmailBody: "Merhaba {{first_name}}"}
	if p := Validate(ok, nil); len(p) != 0 {
		t.Fatalf("exact-limit copy should pass: %v", p)
	}
	bad := Messages{SMSText: strings.Repeat("a", 161), EmailSubject: strings.Repeat("b", 81), EmailBody: "x"}
	p := Validate(bad, nil)
	if len(p) != 2 {
		t.Fatalf("want 2 problems, got %v", p)
	}
}

func TestLimitsCountRunesNotBytes(t *testing.T) {
	m := Messages{SMSText: strings.Repeat("ş", 160), EmailSubject: strings.Repeat("ğ", 80), EmailBody: "x"}
	if p := Validate(m, nil); len(p) != 0 {
		t.Fatalf("160 Turkish runes must fit: %v", p)
	}
}

func TestPersonalisationCountsWorstCaseExpansion(t *testing.T) {
	// 145 chars + the 14-char token = 159 literal (fits), but a 20-char name makes it 165.
	tpl := strings.Repeat("a", 145) + "{{first_name}}"
	m := Messages{SMSText: tpl, EmailSubject: "s", EmailBody: "b"}
	if p := Validate(m, map[string]int{"first_name": 20}); len(p) != 1 || !strings.Contains(p[0], "160") {
		t.Fatalf("expected sms over limit after expansion, got %v", p)
	}
	if p := Validate(m, map[string]int{"first_name": 10}); len(p) != 0 {
		t.Fatalf("shorter names should fit: %v", p)
	}
}

func TestUnknownAndMalformedPlaceholders(t *testing.T) {
	m := Messages{SMSText: "Hi {{phone}}", EmailSubject: "x {{first_name", EmailBody: "ok"}
	p := Validate(m, nil)
	if len(p) != 2 {
		t.Fatalf("want unknown + malformed problems, got %v", p)
	}
}

func TestNormalizeAndRender(t *testing.T) {
	m := Normalize(Messages{SMSText: "  Merhaba {{ first_name }}! ", EmailSubject: "s", EmailBody: "b"})
	if m.SMSText != "Merhaba {{first_name}}!" {
		t.Fatalf("normalize: %q", m.SMSText)
	}
	if got := Render(m.SMSText, map[string]string{"first_name": "Ayşe"}); got != "Merhaba Ayşe!" {
		t.Fatalf("render: %q", got)
	}
}

func TestEmptyAndMultilineSubject(t *testing.T) {
	p := Validate(Messages{SMSText: "", EmailSubject: "a\nb", EmailBody: "x"}, nil)
	if len(p) != 2 {
		t.Fatalf("got %v", p)
	}
}
