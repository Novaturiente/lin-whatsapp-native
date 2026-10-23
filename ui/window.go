// Package ui is the GTK4/libadwaita window. Every function here runs on the
// GTK main thread; work from whatsmeow arrives through glib.IdleAdd.
package ui

import (
	"context"
	"os/exec"
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
.bubble.out { background: alpha(@accent_bg_color, .18); }
.quote { padding: 4px 8px; border-left: 3px solid @accent_color; border-radius: 6px;
	background: alpha(@window_fg_color, .06); }
.sender { font-weight: bold; color: @accent_color; font-size: smaller; }
.meta { font-size: x-small; opacity: .6; }
.reactions { font-size: small; padding: 1px 6px; border-radius: 10px; background: alpha(@window_fg_color, .08); }
.unread { background: @accent_bg_color; color: @accent_fg_color; border-radius: 10px;
	padding: 0 6px; font-size: small; font-weight: bold; }
.conversation { background: alpha(@window_fg_color, .03); }
`

type Window struct {
	win    *adw.ApplicationWindow
	c      *wa.Client
	split  *adw.NavigationSplitView
	toasts *adw.ToastOverlay
	chats  *ChatList
	conv   *Conversation
	qr     *gtk.Picture
}

func New(app *adw.Application, c *wa.Client) *Window {
	provider := gtk.NewCSSProvider()
	provider.LoadFromString(css)
	gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), provider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

	w := &Window{win: adw.NewApplicationWindow(&app.Application), c: c}
	w.win.SetTitle("WhatsApp")
	w.win.SetDefaultSize(1100, 760)
	w.toasts = adw.NewToastOverlay()

	w.chats = newChatList(w)
	w.conv = newConversation(w)
	w.split = adw.NewNavigationSplitView()
	w.split.SetSidebar(adw.NewNavigationPage(w.chats.root, "Chats"))
	w.split.SetContent(adw.NewNavigationPage(w.conv.root, "Chat"))
	w.toasts.SetChild(w.split)

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
	c.OnEvent = func(e wa.Event) { glib.IdleAdd(func() { w.dispatch(e) }) }
	return w
}

func (w *Window) Present() { w.win.Present() }

func (w *Window) bindKeys() {
	keys := gtk.NewEventControllerKey()
	keys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		ctrl := state&gdk.ControlMask != 0
		switch {
		case ctrl && (keyval == gdk.KEY_q || keyval == gdk.KEY_w):
			w.win.Close()
		case ctrl && keyval == gdk.KEY_f:
			w.chats.search.GrabFocus()
		case keyval == gdk.KEY_Escape && w.conv.cancelCompose():
		default:
			return false
		}
		return true
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
			w.toasts.SetChild(w.split)
		}
		w.chats.reload()
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
	go func() {
		icon := w.c.Avatar(e.Chat)
		if icon == "" {
			icon = "lin-whatsapp-native"
		}
		out, err := exec.CommandContext(context.Background(), "notify-send", "-a", "WhatsApp",
			"-i", icon, "-A", "default=Open", title, body).Output()
		if err == nil && strings.TrimSpace(string(out)) == "default" {
			glib.IdleAdd(func() {
				w.openChat(e.Chat)
				w.win.Present()
			})
		}
	}()
}
