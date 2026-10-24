package ui

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
)

// Settings are the look-and-feel choices, saved as JSON in the config dir.
type Settings struct {
	Scheme      int     `json:"scheme"`       // 0 system, 1 light, 2 dark
	Opacity     float64 `json:"opacity"`      // window background, 0.3 to 1
	Font        string  `json:"font"`         // family; "" is the system font
	FontAll     bool    `json:"font_all"`     // the font applies to the whole app, not only messages
	FontSize    float64 `json:"font_size"`    // message text, in points
	OutColor    string  `json:"out_color"`    // own bubbles
	InColor     string  `json:"in_color"`     // others' bubbles; "" follows the theme
	BubbleWidth int     `json:"bubble_width"` // percent of the chat pane
}

func defaults() Settings {
	return Settings{Opacity: opacity(), FontSize: 11.5, OutColor: "#005c4b", BubbleWidth: 75}
}

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "lin-whatsapp-native", "settings.json")
}

// loadSettings fills fields missing from the file with defaults.
func loadSettings() Settings {
	s := defaults()
	if data, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	s.Opacity = min(max(s.Opacity, 0.3), 1)
	s.FontSize = min(max(s.FontSize, 8), 24)
	s.BubbleWidth = min(max(s.BubbleWidth, 40), 95)
	return s
}

func (s Settings) save() error {
	data, _ := json.MarshalIndent(s, "", "  ")
	if err := os.MkdirAll(filepath.Dir(settingsPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath(), data, 0o644)
}

// textOn is a readable text color for a background: dark on light, light on dark.
func textOn(bg string) string {
	c := gdk.NewRGBA(0, 0, 0, 1) // gotk4's zero RGBA has no C struct behind it
	if !c.Parse(bg) {
		return "#e9edef"
	}
	lin := func(v float32) float64 {
		f := float64(v)
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	if 0.2126*lin(c.Red())+0.7152*lin(c.Green())+0.0722*lin(c.Blue()) > 0.18 {
		return "#111b21"
	}
	return "#e9edef"
}

// css is the stylesheet the settings produce, layered over the fixed one.
func (s Settings) css() string {
	var b strings.Builder
	fmt.Fprintf(&b, ".bubble { font-size: %.1fpt; }\n", s.FontSize)
	fmt.Fprintf(&b, ".bubble.out { background: %s; color: %s; }\n", s.OutColor, textOn(s.OutColor))
	if s.InColor != "" {
		fmt.Fprintf(&b, ".bubble.in { background: %s; color: %s; }\n", s.InColor, textOn(s.InColor))
	}
	if s.Font != "" {
		sel := ".bubble"
		if s.FontAll {
			sel = "window"
		}
		fmt.Fprintf(&b, "%s { font-family: %q; }\n", sel, s.Font)
	}
	if s.Opacity < 1 {
		fmt.Fprintf(&b, transparentCSS, s.Opacity)
	}
	return b.String()
}

// applySettings restyles the open window; nothing needs a restart.
func (w *Window) applySettings() {
	w.style.LoadFromString(w.set.css())
	adw.StyleManagerGetDefault().SetColorScheme([]adw.ColorScheme{
		adw.ColorSchemeDefault, adw.ColorSchemeForceLight, adw.ColorSchemeForceDark}[w.set.Scheme])
	w.conv.resizeBubbles()
}

// changed applies and saves after any edit in the dialog.
func (w *Window) changed() {
	w.applySettings()
	if err := w.set.save(); err != nil {
		w.toast("Could not save settings: " + err.Error())
	}
}

func (w *Window) openSettings() {
	d := adw.NewPreferencesDialog()
	d.SetTitle("Settings")
	page := adw.NewPreferencesPage()
	d.Add(page)

	look := adw.NewPreferencesGroup()
	look.SetTitle("Window")
	look.SetDescription("Blur behind the window comes from the compositor (Hyprland's decoration:blur), not the app.")
	scheme := adw.NewComboRow()
	scheme.SetTitle("Theme")
	scheme.SetModel(gtk.NewStringList([]string{"Follow system", "Light", "Dark"}))
	scheme.SetSelected(uint(w.set.Scheme))
	scheme.NotifyProperty("selected", func() { w.set.Scheme = int(scheme.Selected()); w.changed() })
	look.Add(scheme)
	opac := adw.NewSpinRowWithRange(30, 100, 5)
	opac.SetTitle("Background opacity")
	opac.SetSubtitle("Percent; 100 turns see-through off")
	opac.SetValue(math.Round(w.set.Opacity * 100))
	opac.NotifyProperty("value", func() { w.set.Opacity = opac.Value() / 100; w.changed() })
	look.Add(opac)
	page.Add(look)

	text := adw.NewPreferencesGroup()
	text.SetTitle("Text")
	font := adw.NewActionRow()
	font.SetTitle("Font")
	fontBtn := gtk.NewFontDialogButton(gtk.NewFontDialog())
	fontBtn.SetLevel(gtk.FontLevelFamily)
	fontBtn.SetVAlign(gtk.AlignCenter)
	if w.set.Font != "" {
		fontBtn.SetFontDesc(pango.FontDescriptionFromString(w.set.Font))
	} else {
		fontBtn.SetFontDesc(pango.FontDescriptionFromString(systemFont()))
	}
	fontBtn.NotifyProperty("font-desc", func() {
		if fd := fontBtn.FontDesc(); fd != nil {
			w.set.Font = fd.Family()
			w.changed()
		}
	})
	font.AddSuffix(fontBtn)
	text.Add(font)
	all := adw.NewSwitchRow()
	all.SetTitle("Use the font everywhere")
	all.SetSubtitle("Off: only message text")
	all.SetActive(w.set.FontAll)
	all.NotifyProperty("active", func() { w.set.FontAll = all.Active(); w.changed() })
	text.Add(all)
	size := adw.NewSpinRowWithRange(8, 24, 0.5)
	size.SetDigits(1)
	size.SetTitle("Message text size")
	size.SetSubtitle("Points")
	size.SetValue(w.set.FontSize)
	size.NotifyProperty("value", func() { w.set.FontSize = size.Value(); w.changed() })
	text.Add(size)
	page.Add(text)

	bub := adw.NewPreferencesGroup()
	bub.SetTitle("Bubbles")
	bub.Add(w.colorRow("Your messages", &w.set.OutColor, "#005c4b"))
	bub.Add(w.colorRow("Others' messages", &w.set.InColor, "#202c33"))
	width := adw.NewSpinRowWithRange(40, 95, 5)
	width.SetTitle("Maximum width")
	width.SetSubtitle("Percent of the chat pane")
	width.SetValue(float64(w.set.BubbleWidth))
	width.NotifyProperty("value", func() { w.set.BubbleWidth = int(width.Value()); w.changed() })
	bub.Add(width)
	page.Add(bub)

	reset := adw.NewPreferencesGroup()
	btn := gtk.NewButtonWithLabel("Reset to defaults")
	btn.AddCSSClass("destructive-action")
	btn.SetHAlign(gtk.AlignCenter)
	btn.ConnectClicked(func() {
		w.set = defaults()
		w.changed()
		d.Close()
		w.openSettings() // reopen so every control shows the defaults
	})
	reset.Add(btn)
	page.Add(reset)

	d.Present(w.win)
}

// colorRow edits one color setting. shown is what the button displays
// while the setting is empty (following the theme).
func (w *Window) colorRow(title string, field *string, shown string) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(title)
	cd := gtk.NewColorDialog()
	cd.SetWithAlpha(false)
	btn := gtk.NewColorDialogButton(cd)
	btn.SetVAlign(gtk.AlignCenter)
	c := gdk.NewRGBA(0, 0, 0, 1)
	if !c.Parse(*field) {
		c.Parse(shown)
	}
	btn.SetRGBA(&c)
	btn.NotifyProperty("rgba", func() {
		if rgba := btn.RGBA(); rgba != nil {
			*field = rgba.String()
			w.changed()
		}
	})
	row.AddSuffix(btn)
	return row
}

func systemFont() string {
	if s := gtk.SettingsGetDefault(); s != nil {
		if v := s.ObjectProperty("gtk-font-name"); v != nil {
			if name, ok := v.(string); ok {
				return name
			}
		}
	}
	return "Sans 11"
}
