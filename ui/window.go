// Package ui is the GTK4/libadwaita window. Every function here runs on the
// GTK main thread; work from whatsmeow arrives through glib.IdleAdd.
package ui

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"rsc.io/qr"

	"lin-whatsapp-native/wa"
)

const css = `
.bubble { padding: 6px 10px; border-radius: 12px; }
.bubble.in { background: alpha(@card_bg_color, 1); box-shadow: 0 1px 1px alpha(black, .08); }
.bubble.out .quote { background: alpha(black, .18); }
.bubble.out .meta { opacity: .75; }
.bubble label { line-height: 1.35; }
.slot { border-radius: 8px; transition: background 500ms ease-out; }
.slot.flash { background: alpha(@accent_bg_color, .35); transition: none; }
.slot.cursor { background: alpha(@window_fg_color, .10); }
searchbar.chat-search > revealer > box { padding: 4px 8px; }
.quote { padding: 4px 8px; border-left: 3px solid @accent_color; border-radius: 6px;
	background: alpha(@window_fg_color, .06); }
.sender { font-weight: bold; color: @accent_color; font-size: smaller; }
.meta { font-size: x-small; opacity: .6; }
.reactions { font-size: small; padding: 1px 6px; border-radius: 10px; background: alpha(@window_fg_color, .08); }
.unread { background: @accent_bg_color; color: @accent_fg_color; border-radius: 10px;
	padding: 0 6px; font-size: small; font-weight: bold; }
.conversation { background: alpha(@window_fg_color, .03); }
.system { font-size: small; padding: 3px 10px; margin: 4px 0; border-radius: 8px;
	background: alpha(@window_fg_color, .07); opacity: .85; }
`

// transparentCSS clears every pane's own background, so only the window's
// translucent one shows; bubbles, the composer and popovers stay solid.
// %.2f is the window's opacity. Scoped to the main window (class "main"):
// the media viewer keeps a solid background behind photos.
const transparentCSS = `
window.main.background { background-color: alpha(@window_bg_color, %.2f); }
window.main toolbarview, window.main headerbar, window.main .navigation-sidebar,
window.main list, window.main scrolledwindow, window.main viewport,
window.main navigation-split-view > *, window.main .sidebar-pane, window.main .content-pane,
window.main statuspage, window.main .conversation, window.main searchbar > revealer > box,
window.main banner > revealer > widget {
	background: transparent; box-shadow: none; }
window.main headerbar { border-bottom: 1px solid alpha(@window_fg_color, .08); }
`

// opacity is LINWA_OPACITY (0.3 to 1), the default background opacity
// until Settings saves one. 1 turns transparency off.
func opacity() float64 {
	v, err := strconv.ParseFloat(os.Getenv("LINWA_OPACITY"), 64)
	if err != nil {
		return 0.5 // half see-through; the compositor's blur does the rest
	}
	return min(max(v, 0.3), 1)
}

type Window struct {
	win    *adw.ApplicationWindow
	c      *wa.Client
	split  *adw.NavigationSplitView
	main   *gtk.Box // connection banner above the split view
	banner *adw.Banner
	toasts *adw.ToastOverlay
	chats  *ChatList
	conv   *Conversation
	qr     *gtk.Picture
	set    Settings
	style  *gtk.CSSProvider // generated from set; see applySettings
}

func New(app *adw.Application, c *wa.Client) *Window {
	provider := gtk.NewCSSProvider()
	provider.LoadFromString(css)
	gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), provider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

	w := &Window{win: adw.NewApplicationWindow(&app.Application), c: c,
		set: loadSettings(), style: gtk.NewCSSProvider()}
	gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), w.style, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION+1)
	w.win.SetTitle("WhatsApp")
	w.win.AddCSSClass("main")
	w.win.SetDefaultSize(1100, 760)
	w.toasts = adw.NewToastOverlay()

	w.chats = newChatList(w)
	w.conv = newConversation(w)
	w.split = adw.NewNavigationSplitView()
	w.split.SetSidebar(adw.NewNavigationPage(w.chats.root, "Chats"))
	w.split.SetContent(adw.NewNavigationPage(w.conv.root, "Chat"))
	w.split.SetVExpand(true)
	w.split.SetMinSidebarWidth(320) // libadwaita's 180-280 px default crams names and previews
	w.split.SetMaxSidebarWidth(420)
	w.split.SetSidebarWidthFraction(0.3)
	w.banner = adw.NewBanner("Connecting…")
	w.banner.SetRevealed(true) // until the first Connected
	w.main = gtk.NewBox(gtk.OrientationVertical, 0)
	w.main.Append(w.banner)
	w.main.Append(w.split)
	w.toasts.SetChild(w.main)

	// Narrow window: show one pane at a time.
	bp := adw.NewBreakpoint(adw.NewBreakpointConditionLength(adw.BreakpointConditionMaxWidth, 640, adw.LengthUnitSp))
	bp.AddSetter(w.split, "collapsed", true)
	w.win.AddBreakpoint(bp)

	w.win.SetContent(w.toasts)
	w.win.NotifyProperty("is-active", func() {
		active := w.win.IsActive()
		c.Online(active)
		if active && w.conv.chat != "" {
			c.MarkRead(w.conv.chat)
			w.chats.reload()
		}
	})
	w.bindKeys()
	w.applySettings()
	c.OnEvent = func(e wa.Event) { glib.IdleAdd(func() { w.dispatch(e) }) }
	return w
}

func (w *Window) Present() { w.win.Present() }

func (w *Window) bindKeys() {
	keys := gtk.NewEventControllerKey()
	keys.SetPropagationPhase(gtk.PhaseCapture) // ahead of the text box, which would eat Ctrl+K
	keys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		return w.windowKey(keyval, state)
	})
	w.win.AddController(keys)
}

func (w *Window) toast(msg string) { w.toasts.AddToast(adw.NewToast(msg)) }

func (w *Window) dispatch(e wa.Event) {
	switch e.Kind {
	case wa.EvQR:
		w.showQR(e.QR)
	case wa.EvConnected:
		if w.qr != nil {
			w.qr = nil
			w.toasts.SetChild(w.main)
		}
		w.chats.reload()
	case wa.EvConnection:
		w.banner.SetTitle(e.Text)
		w.banner.SetRevealed(e.Text != "")
	case wa.EvHistory:
		if e.Chat == w.conv.chat {
			w.conv.historyArrived()
		}
	case wa.EvLoggedOut:
		w.toast("This device was unlinked. Restart the app to link it again.")
	case wa.EvMessage:
		w.chats.reload()
		if e.Chat == w.conv.chat {
			w.conv.add(e.ID)
			if w.win.IsActive() {
				w.c.MarkRead(e.Chat)
			}
		}
		if e.Notify != nil && (!w.win.IsActive() || e.Chat != w.conv.chat) {
			w.notify(e)
		}
	case wa.EvMissed:
		if !w.win.IsActive() {
			title := "WhatsApp"
			if e.Chat != "" {
				title = w.c.Name(e.Chat)
			}
			w.popup(e.Chat, title, e.Text)
		}
	case wa.EvUpdate:
		if e.Chat == w.conv.chat {
			w.conv.update(e.ID)
		}
		w.chats.reload()
	case wa.EvChats:
		w.chats.reload()
		w.conv.refreshTitle()
	case wa.EvError:
		w.toast(e.Text)
	case wa.EvTyping:
		if e.Chat == w.conv.chat {
			w.conv.typing(e.Sender, e.Typing)
		}
	}
}

func (w *Window) showQR(code string) {
	png, err := qr.Encode(code, qr.L)
	if err != nil {
		return
	}
	png.Scale = 8
	tex, err := gdk.NewTextureFromBytes(glib.NewBytes(png.PNG()))
	if err != nil {
		return
	}
	if w.qr == nil {
		page := adw.NewStatusPage()
		page.SetTitle("Link this computer")
		page.SetDescription("On your phone open WhatsApp, then Settings, Linked devices, Link a device, and scan this code.")
		w.qr = gtk.NewPictureForPaintable(tex)
		w.qr.SetSizeRequest(300, 300)
		page.SetChild(w.qr)
		w.toasts.SetChild(page)
		return
	}
	w.qr.SetPaintable(tex)
}

func (w *Window) openChat(chat string) {
	w.conv.open(chat)
	w.split.SetShowContent(true)
	w.c.MarkRead(chat)
	w.chats.reload()
}

// notify shows a desktop notification; clicking it opens the chat.
func (w *Window) notify(e wa.Event) {
	m := e.Notify
	title := w.c.Name(e.Chat)
	body := previewOf(m)
	if strings.HasSuffix(e.Chat, "@g.us") {
		body = w.c.Name(m.Sender) + ": " + body
	}
	w.popup(e.Chat, title, body)
}

// popup runs notify-send; clicking opens chat, or just raises the window when chat is "".
func (w *Window) popup(chat, title, body string) {
	go func() {
		args := []string{"-a", "WhatsApp", "-i", "io.github.nova.LinWhatsappNative", "-A", "default=Open"}
		// The picture goes in the image-path hint: shells such as quickshell
		// show it large, while -i is only the small app icon.
		if chat != "" {
			if pic := w.c.Avatar(chat); pic != "" {
				args = append(args, "-h", "string:image-path:file://"+pic)
			}
		}
		out, err := exec.CommandContext(context.Background(), "notify-send",
			append(args, title, body)...).Output()
		if err == nil && strings.TrimSpace(string(out)) == "default" {
			glib.IdleAdd(func() {
				if chat != "" {
					w.openChat(chat)
				}
				w.win.Present()
			})
		}
	}()
}
