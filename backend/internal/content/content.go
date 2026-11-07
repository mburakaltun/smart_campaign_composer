// Package content validates and renders the SMS / email copy Claude writes.
package content

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	SMSMax     = 160
	SubjectMax = 80
	BodyMax    = 5000
)

// Placeholders maps each personalisation variable to its (trusted) SQL column.
var Placeholders = map[string]string{
	"first_name": "u.first_name",
	"last_name":  "u.last_name",
	"city":       "u.city",
}

func PlaceholderNames() []string {
	names := make([]string, 0, len(Placeholders))
	for k := range Placeholders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

type Messages struct {
	SMSText      string `json:"sms_text"`
	EmailSubject string `json:"email_subject"`
	EmailBody    string `json:"email_body"`
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// Normalize trims the copy and canonicalises "{{ first_name }}" to "{{first_name}}".
func Normalize(m Messages) Messages {
	n := func(s string) string {
		return placeholderRe.ReplaceAllString(strings.TrimSpace(s), "{{$1}}")
	}
	return Messages{SMSText: n(m.SMSText), EmailSubject: n(m.EmailSubject), EmailBody: n(m.EmailBody)}
}

// RenderedLen is the length (in characters) of tpl once every placeholder is
// replaced by the longest value present in the audience. If this fits the
// limit, every recipient's rendered message fits too.
func RenderedLen(tpl string, maxLen map[string]int) int {
	total := len([]rune(tpl))
	for _, m := range placeholderRe.FindAllStringSubmatch(tpl, -1) {
		total += maxLen[m[1]] - len([]rune(m[0]))
	}
	return total
}

// Validate returns human-readable problems (also fed back to Claude for repair).
// maxLen may be nil, in which case placeholders count as their literal text.
func Validate(m Messages, maxLen map[string]int) []string {
	var p []string
	check := func(label, s string, limit int, single bool) {
		if s == "" {
			p = append(p, label+" is empty")
			return
		}
		for _, match := range placeholderRe.FindAllStringSubmatch(s, -1) {
			if _, ok := Placeholders[match[1]]; !ok {
				p = append(p, fmt.Sprintf("%s uses unknown placeholder {{%s}} (allowed: %s)", label, match[1], strings.Join(PlaceholderNames(), ", ")))
			}
		}
		stripped := placeholderRe.ReplaceAllString(s, "")
		if strings.Contains(stripped, "{{") || strings.Contains(stripped, "}}") {
			p = append(p, label+" contains a malformed placeholder")
		}
		if single && strings.ContainsAny(s, "\r\n") {
			p = append(p, label+" must be a single line")
		}
		n := len([]rune(s))
		if maxLen != nil {
			n = RenderedLen(s, maxLen)
		}
		if n > limit {
			p = append(p, fmt.Sprintf("%s is %d characters after personalisation, limit is %d", label, n, limit))
		}
	}
	check("sms_text", m.SMSText, SMSMax, true)
	check("email_subject", m.EmailSubject, SubjectMax, true)
	check("email_body", m.EmailBody, BodyMax, false)
	return p
}

// Render substitutes placeholders; unknown placeholders are left untouched.
func Render(tpl string, vals map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(tpl, func(s string) string {
		name := placeholderRe.FindStringSubmatch(s)[1]
		if v, ok := vals[name]; ok {
			return v
		}
		return s
	})
}
