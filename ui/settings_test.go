package ui

import (
	"strings"
	"testing"
)

func TestTextOn(t *testing.T) {
	for bg, want := range map[string]string{"#005c4b": "#e9edef", "#ffffff": "#111b21", "#dcf8c6": "#111b21", "bogus": "#e9edef"} {
		if got := textOn(bg); got != want {
			t.Errorf("textOn(%s) = %s, want %s", bg, got, want)
		}
	}
}

func TestSettingsCSS(t *testing.T) {
	s := Settings{Opacity: 1, FontSize: 12, OutColor: "#ffffff", Font: "Inter", BubbleWidth: 75}
	css := s.css()
	for _, want := range []string{"font-size: 12.0pt", "background: #ffffff; color: #111b21", `.bubble { font-family: "Inter"; }`} {
		if !strings.Contains(css, want) {
			t.Errorf("css lacks %q:\n%s", want, css)
		}
	}
	if strings.Contains(css, "transparent") || strings.Contains(css, ".bubble.in") {
		t.Errorf("opacity 1 or empty in-color leaked rules:\n%s", css)
	}
	s.FontAll, s.Opacity = true, 0.5
	if css = s.css(); !strings.Contains(css, `window { font-family: "Inter"; }`) || !strings.Contains(css, "0.50") {
		t.Errorf("whole-app font or opacity missing:\n%s", css)
	}
}
