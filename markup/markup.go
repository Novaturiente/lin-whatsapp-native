// Package markup turns WhatsApp message text into Pango markup: links,
// @mentions, and WhatsApp's *bold*, _italic_, ~strike~ and ```mono```.
package markup

import (
	"html"
	"regexp"
	"strings"
)

var (
	link    = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s<>"]+[^\s<>".,;:!?)\]'"]`)
	mention = regexp.MustCompile(`@(\d{5,})`)
	styles  = []struct {
		re  *regexp.Regexp
		tag string
	}{
		{regexp.MustCompile("```([^`]+?)```"), "tt"},
		{regexp.MustCompile(`(^|[\s(])\*([^*\n]+?)\*($|[\s.,!?)])`), "b"},
		{regexp.MustCompile(`(^|[\s(])_([^_\n]+?)_($|[\s.,!?)])`), "i"},
		{regexp.MustCompile(`(^|[\s(])~([^~\n]+?)~($|[\s.,!?)])`), "s"},
	}
)

// Pango converts text to markup. name resolves a mentioned phone number to
// the name to show; mentions lists the numbers the sender really tagged, so
// a number someone merely typed stays a number.
func Pango(text string, mentions []string, name func(jid string) string) string {
	tagged := map[string]string{}
	for _, j := range mentions {
		tagged[strings.SplitN(j, "@", 2)[0]] = j
	}
	var out strings.Builder
	last := 0
	for _, loc := range link.FindAllStringIndex(text, -1) {
		out.WriteString(plain(text[last:loc[0]], tagged, name))
		url := text[loc[0]:loc[1]]
		href := url
		if !strings.Contains(strings.ToLower(href), "://") {
			href = "https://" + href
		}
		out.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(url) + `</a>`)
		last = loc[1]
	}
	out.WriteString(plain(text[last:], tagged, name))
	return out.String()
}

func plain(s string, tagged map[string]string, name func(string) string) string {
	s = html.EscapeString(s)
	for _, st := range styles {
		if st.tag == "tt" {
			s = st.re.ReplaceAllString(s, "<tt>$1</tt>")
			continue
		}
		// run twice: adjacent matches share the space between them
		for range 2 {
			s = st.re.ReplaceAllString(s, "$1<"+st.tag+">$2</"+st.tag+">$3")
		}
	}
	return mention.ReplaceAllStringFunc(s, func(m string) string {
		if j, ok := tagged[m[1:]]; ok {
			return "<b>@" + html.EscapeString(name(j)) + "</b>"
		}
		return m
	})
}
