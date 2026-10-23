package ui

import (
	"fmt"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"lin-whatsapp-native/store"
)

// viewer shows a chat's photos and videos one at a time, large, inside the
// app: arrows step through them, Esc closes, click toggles fit and 1:1.
type viewer struct {
	c     *Conversation
	win   *adw.Window
	title *adw.WindowTitle
	body  *gtk.ScrolledWindow
	items []*store.Message
	i     int
	fit   bool
	copy  *gtk.Button
	video *gtk.Video
}

func (c *Conversation) view(m *store.Message) {
	items, _ := c.w.c.Store.Media(m.Chat)
	v := &viewer{c: c, win: adw.NewWindow(), title: adw.NewWindowTitle("", ""),
		body: gtk.NewScrolledWindow(), items: items, i: -1, fit: true}
	for i, it := range items {
		if it.ID == m.ID {
			v.i = i
		}
	}
	if v.i < 0 { // older than the newest 500
		v.items, v.i = []*store.Message{m}, 0
	}

	header := adw.NewHeaderBar()
	header.SetTitleWidget(v.title)
	button := func(icon, tip string, f func()) *gtk.Button {
		b := gtk.NewButtonFromIconName(icon)
		b.SetTooltipText(tip)
		b.ConnectClicked(f)
		return b
	}
	header.PackStart(button("go-previous-symbolic", "Previous (←)", func() { v.step(-1) }))
	header.PackStart(button("go-next-symbolic", "Next (→)", func() { v.step(1) }))
	header.PackEnd(button("document-save-symbolic", "Save to Downloads (Ctrl+S)", func() { c.save(v.cur()) }))
	v.copy = button("edit-copy-symbolic", "Copy image (Ctrl+C)", func() { c.copyImage(v.cur()) })
	header.PackEnd(v.copy)
	header.PackEnd(button("document-open-symbolic", "Open in another app", func() { c.openFile(v.cur()) }))
	header.PackEnd(button("mail-reply-sender-symbolic", "Reply", func() { v.win.Close(); c.composer.replyTo(v.cur()) }))

	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(header)
	toolbar.SetContent(v.body)
	v.body.SetVExpand(true)
	v.win.SetContent(toolbar)
	v.win.SetTransientFor(&c.w.win.Window)
	v.win.SetModal(true)
	v.win.SetDefaultSize(1000, 750)
	v.win.Maximize()

	keys := gtk.NewEventControllerKey()
	keys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		ctrl := state&gdk.ControlMask != 0
		switch {
		case keyval == gdk.KEY_Escape:
			v.win.Close()
		case keyval == gdk.KEY_Left:
			v.step(-1)
		case keyval == gdk.KEY_Right:
			v.step(1)
		case ctrl && keyval == gdk.KEY_s:
			c.save(v.cur())
		case ctrl && keyval == gdk.KEY_c && v.cur().Kind == store.KindImage:
			c.copyImage(v.cur())
		case keyval == gdk.KEY_space && v.video != nil:
			if s := v.video.MediaStream(); s != nil {
				b := gtk.BaseMediaStream(s)
				b.SetPlaying(!b.Playing())
			}
		default:
			return false
		}
		return true
	})
	v.win.AddController(keys)
	v.win.ConnectCloseRequest(func() bool { // the stream can outlive the window for a moment
		if v.video != nil && v.video.MediaStream() != nil {
			gtk.BaseMediaStream(v.video.MediaStream()).Pause()
		}
		return false
	})
	v.show()
	v.win.Present()
}

func (v *viewer) cur() *store.Message { return v.items[v.i] }

func (v *viewer) step(d int) {
	if j := v.i + d; j >= 0 && j < len(v.items) {
		v.i, v.fit = j, true
		v.show()
	}
}

func (v *viewer) show() {
	m := v.cur()
	who := v.c.w.c.Name(m.Sender)
	v.title.SetTitle(who)
	v.title.SetSubtitle(fmt.Sprintf("%s · %d of %d", m.Time.Format("2 Jan 2006, 15:04"), v.i+1, len(v.items)))
	v.copy.SetVisible(m.Kind == store.KindImage)
	if v.video != nil { // stop the previous video's sound
		if s := v.video.MediaStream(); s != nil {
			gtk.BaseMediaStream(s).Pause()
		}
		v.video = nil
	}
	wait := gtk.NewSpinner()
	wait.Start()
	wait.SetSizeRequest(48, 48)
	wait.SetHAlign(gtk.AlignCenter)
	wait.SetVAlign(gtk.AlignCenter)
	v.body.SetChild(wait)
	v.c.w.c.Fetch(m, func(path string, err error) {
		glib.IdleAdd(func() {
			if v.cur() != m { // the reader moved on while this downloaded
				return
			}
			if err != nil {
				v.body.SetChild(italic("Could not download: " + err.Error()))
				return
			}
			if m.Kind == store.KindVideo {
				v.video = gtk.NewVideoForFilename(path)
				v.video.SetAutoplay(true)
				v.video.SetLoop(m.Animated)
				v.body.SetChild(v.video)
				return
			}
			v.picture(m, path)
		})
	})
}

func (v *viewer) picture(m *store.Message, path string) {
	pic := gtk.NewPicture()
	pic.SetContentFit(gtk.ContentFitContain)
	apply := func() {
		pic.SetCanShrink(v.fit) // off: natural size, scroll to pan
		if v.fit {
			v.body.SetPolicy(gtk.PolicyNever, gtk.PolicyNever)
		} else {
			v.body.SetPolicy(gtk.PolicyAutomatic, gtk.PolicyAutomatic)
		}
	}
	apply()
	click := gtk.NewGestureClick()
	click.ConnectReleased(func(int, float64, float64) { v.fit = !v.fit; apply() })
	pic.AddController(click)
	pic.SetTooltipText("Click to zoom")
	v.body.SetChild(pic)
	texture(path, 4096, func(t *gdk.MemoryTexture) {
		if v.cur() == m {
			pic.SetPaintable(t)
		}
	})
}
