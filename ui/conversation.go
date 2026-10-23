package ui

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"lin-whatsapp-native/markup"
	"lin-whatsapp-native/media"
	"lin-whatsapp-native/store"
)

const pageSize = 60

type Conversation struct {
	w        *Window
	root     *adw.ToolbarView
	title    *adw.WindowTitle
	avatar   *adw.Avatar
	stack    *gtk.Stack
	scroll   *gtk.ScrolledWindow
	box      *gtk.Box
	chat     string
	slots    map[string]*gtk.Box // message ID to the row holding its bubble
	oldest   *store.Message
	loading  bool
	composer *Composer
	typers   map[string]time.Time
}

func newConversation(w *Window) *Conversation {
	c := &Conversation{w: w, root: adw.NewToolbarView(), title: adw.NewWindowTitle("", ""),
		stack: gtk.NewStack(), scroll: gtk.NewScrolledWindow(), box: gtk.NewBox(gtk.OrientationVertical, 4),
		slots: map[string]*gtk.Box{}, typers: map[string]time.Time{}}
	header := adw.NewHeaderBar()
	c.avatar = adw.NewAvatar(32, "", true)
	header.PackStart(c.avatar)
	header.SetTitleWidget(c.title)
	c.root.AddTopBar(header)

	c.box.SetMarginStart(12)
	c.box.SetMarginEnd(12)
	c.box.SetMarginTop(8)
	c.box.SetMarginBottom(8)
	c.box.SetVAlign(gtk.AlignEnd)
	c.scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	c.scroll.SetChild(c.box)
	c.scroll.SetVExpand(true)
	c.scroll.AddCSSClass("conversation")
	c.scroll.VAdjustment().ConnectValueChanged(func() {
		if c.scroll.VAdjustment().Value() < 300 {
			c.loadOlder()
		}
	})

	empty := adw.NewStatusPage()
	empty.SetIconName("chat-bubbles-empty-symbolic")
	empty.SetTitle("Pick a chat")
	c.stack.AddNamed(empty, "empty")
	c.stack.AddNamed(c.scroll, "chat")
	c.root.SetContent(c.stack)

	c.composer = newComposer(c)
	c.root.AddBottomBar(c.composer.root)
	c.composer.root.SetVisible(false)

	// Files dropped anywhere on the conversation are sent.
	drop := gtk.NewDropTarget(gdk.GTypeFileList, gdk.ActionCopy)
	drop.ConnectDrop(func(v *glib.Value, _, _ float64) bool {
		list, ok := v.GoValue().(*gdk.FileList)
		if !ok || c.chat == "" {
			return false
		}
		for _, f := range list.Files() {
			c.composer.sendFile(f.Path())
		}
		return true
	})
	c.stack.AddController(drop)
	return c
}

func (c *Conversation) open(chat string) {
	if chat == c.chat {
		return
	}
	c.composer.stopTyping() // tell the old chat, before c.chat moves on
	c.chat, c.oldest, c.typers = chat, nil, map[string]time.Time{}
	for child := c.box.FirstChild(); child != nil; child = c.box.FirstChild() {
		c.box.Remove(child)
	}
	c.slots = map[string]*gtk.Box{}
	c.stack.SetVisibleChildName("chat")
	c.composer.root.SetVisible(true)
	c.composer.reset()
	c.refreshTitle()
	page, _ := c.w.c.Store.Messages(chat, nil, pageSize)
	for _, m := range page {
		c.box.Append(c.slot(m))
	}
	if len(page) > 0 {
		c.oldest = page[0]
	}
	c.scrollToEnd()
	c.composer.focus()
}

func (c *Conversation) refreshTitle() {
	if c.chat == "" {
		return
	}
	name := c.w.c.Name(c.chat)
	c.title.SetTitle(name)
	c.avatar.SetText(name)
	if tex := c.w.chats.avatars[c.chat]; tex != nil {
		c.avatar.SetCustomImage(tex)
	} else {
		c.avatar.SetCustomImage(nil)
	}
	c.showTyping()
}

func (c *Conversation) typing(sender string, on bool) {
	if on {
		c.typers[sender] = time.Now()
		glib.TimeoutSecondsAdd(7, func() bool { c.showTyping(); return false })
	} else {
		delete(c.typers, sender)
	}
	c.showTyping()
}

// showTyping drops typers not heard from in 6 s; WhatsApp sends no "stopped"
// when someone just leaves.
func (c *Conversation) showTyping() {
	var names []string
	for s, at := range c.typers {
		if time.Since(at) > 6*time.Second {
			delete(c.typers, s)
			continue
		}
		names = append(names, c.w.c.Name(s))
	}
	switch {
	case len(names) == 0:
		c.title.SetSubtitle("")
	case isGroup(c.chat):
		sort.Strings(names)
		c.title.SetSubtitle(strings.Join(names, ", ") + " typing…")
	default:
		c.title.SetSubtitle("typing…")
	}
}

func isGroup(jid string) bool { return strings.HasSuffix(jid, "@g.us") }

func (c *Conversation) nearEnd() bool {
	a := c.scroll.VAdjustment()
	return a.Value()+a.PageSize() >= a.Upper()-150
}

func (c *Conversation) scrollToEnd() {
	// twice: images finish sizing a frame later and push the end further
	for _, ms := range []uint{30, 300} {
		glib.TimeoutAdd(ms, func() bool {
			a := c.scroll.VAdjustment()
			a.SetValue(a.Upper())
			return false
		})
	}
}

func (c *Conversation) loadOlder() {
	if c.loading || c.oldest == nil {
		return
	}
	page, _ := c.w.c.Store.Messages(c.chat, c.oldest, pageSize)
	if len(page) == 0 {
		c.oldest = nil
		return
	}
	c.loading = true
	a := c.scroll.VAdjustment()
	upper := a.Upper()
	for i := len(page) - 1; i >= 0; i-- {
		c.box.Prepend(c.slot(page[i]))
	}
	c.oldest = page[0]
	glib.IdleAdd(func() { // keep the message under the reader where it was
		a.SetValue(a.Value() + a.Upper() - upper)
		c.loading = false
	})
}

// add shows a message that just arrived or was just sent.
func (c *Conversation) add(id string) {
	if _, ok := c.slots[id]; ok {
		c.update(id)
		return
	}
	m, err := c.w.c.Store.Message(c.chat, id)
	if err != nil || m == nil {
		return
	}
	end := c.nearEnd()
	c.box.Append(c.slot(m))
	delete(c.typers, m.Sender)
	c.showTyping()
	if end || m.FromMe {
		c.scrollToEnd()
	}
}

func (c *Conversation) update(id string) {
	slot, ok := c.slots[id]
	if !ok {
		return
	}
	m, err := c.w.c.Store.Message(c.chat, id)
	if err != nil || m == nil {
		return
	}
	if old := slot.FirstChild(); old != nil {
		slot.Remove(old)
	}
	slot.Append(c.bubble(m))
}

func (c *Conversation) slot(m *store.Message) *gtk.Box {
	slot := gtk.NewBox(gtk.OrientationHorizontal, 0)
	slot.Append(c.bubble(m))
	c.slots[m.ID] = slot
	return slot
}

// bubble draws one message.
func (c *Conversation) bubble(m *store.Message) gtk.Widgetter {
	col := gtk.NewBox(gtk.OrientationVertical, 3)
	col.AddCSSClass("bubble")
	if m.FromMe {
		col.AddCSSClass("out")
		col.SetHAlign(gtk.AlignEnd)
		col.SetMarginStart(80)
	} else {
		col.AddCSSClass("in")
		col.SetHAlign(gtk.AlignStart)
		col.SetMarginEnd(80)
	}
	col.SetHExpand(true)
	if m.Kind == store.KindSticker && !m.Deleted {
		col.RemoveCSSClass("bubble")
	}

	if isGroup(m.Chat) && !m.FromMe {
		name := gtk.NewLabel(c.w.c.Name(m.Sender))
		name.SetXAlign(0)
		name.AddCSSClass("sender")
		col.Append(name)
	}
	if m.QuoteID != "" && !m.Deleted {
		col.Append(c.quote(m))
	}
	c.content(col, m)
	col.Append(c.meta(m))

	outer := gtk.NewBox(gtk.OrientationVertical, 0)
	outer.SetHExpand(true)
	outer.Append(col)
	if r := c.reactions(m); r != nil {
		if m.FromMe {
			r.SetHAlign(gtk.AlignEnd)
		} else {
			r.SetHAlign(gtk.AlignStart)
		}
		outer.Append(r)
	}

	click := gtk.NewGestureClick()
	click.SetButton(gdk.BUTTON_SECONDARY)
	click.ConnectPressed(func(_ int, x, y float64) { c.menu(col, m, x, y) })
	col.AddController(click)
	press := gtk.NewGestureLongPress() // touchpads and touch screens
	press.ConnectPressed(func(x, y float64) { c.menu(col, m, x, y) })
	col.AddController(press)
	return outer
}

func validMarkup(s string) bool {
	_, _, _, err := pango.ParseMarkup(s, 0)
	return err == nil
}

func italic(text string) *gtk.Label {
	l := gtk.NewLabel(text)
	l.SetXAlign(0)
	l.SetWrap(true)
	l.AddCSSClass("dim-label")
	l.SetMarkup("<i>" + glib.MarkupEscapeText(text) + "</i>")
	return l
}

func (c *Conversation) textLabel(m *store.Message) *gtk.Label {
	l := gtk.NewLabel("")
	if text := markup.Pango(m.Text, m.Mentions, c.w.c.Name); validMarkup(text) {
		l.SetMarkup(text)
	} else { // crossed styles like "*a _b* c_" make tags Pango rejects
		l.SetText(m.Text)
	}
	l.SetXAlign(0)
	l.SetWrap(true)
	l.SetWrapMode(pango.WrapWordChar)
	l.SetSelectable(true)
	l.SetCanFocus(false) // selectable labels grab focus and select all
	l.SetMaxWidthChars(60)
	return l
}

func (c *Conversation) content(col *gtk.Box, m *store.Message) {
	switch {
	case m.Deleted:
		col.Append(italic("🚫 This message was deleted"))
		return
	case m.ViewOnce:
		col.Append(italic("View-once message. Open it on your phone."))
		return
	}
	switch m.Kind {
	case store.KindImage:
		col.Append(c.picture(m, 320))
	case store.KindSticker:
		col.Append(c.picture(m, 160))
		return
	case store.KindVideo:
		col.Append(c.video(m))
	case store.KindAudio:
		col.Append(c.audio(m))
	case store.KindDocument:
		col.Append(c.document(m))
	case store.KindPoll:
		col.Append(c.poll(m))
		return
	case store.KindContact:
		col.Append(italic("👤 Contact: " + m.Text))
		return
	case store.KindOther:
		col.Append(italic("This message type is not supported here. Open it on your phone."))
		return
	}
	if m.Text != "" {
		col.Append(c.textLabel(m))
	}
}

func (c *Conversation) quote(m *store.Message) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 1)
	box.AddCSSClass("quote")
	who := gtk.NewLabel(c.w.c.Name(m.QuoteSender))
	who.SetXAlign(0)
	who.AddCSSClass("sender")
	text := gtk.NewLabel(firstLine(m.QuoteText))
	text.SetXAlign(0)
	text.SetEllipsize(pango.EllipsizeEnd)
	text.SetMaxWidthChars(50)
	box.Append(who)
	box.Append(text)
	click := gtk.NewGestureClick()
	click.ConnectReleased(func(int, float64, float64) { c.jumpTo(m.QuoteID) })
	box.AddController(click)
	return box
}

// jumpTo scrolls to a loaded message, loading older pages until it appears.
func (c *Conversation) jumpTo(id string) {
	for range 20 {
		if slot, ok := c.slots[id]; ok {
			glib.IdleAdd(func() {
				if r, ok := slot.ComputeBounds(c.box); ok {
					c.scroll.VAdjustment().SetValue(float64(r.Y()) - 80)
				}
			})
			return
		}
		if c.oldest == nil {
			return
		}
		c.loading = false
		c.loadOlder()
	}
}

func (c *Conversation) meta(m *store.Message) gtk.Widgetter {
	t := m.Time.Format("15:04")
	if now := time.Now(); m.Time.YearDay() != now.YearDay() || m.Time.Year() != now.Year() {
		t = m.Time.Format("2 Jan 2006, 15:04")
	}
	if m.Edited && !m.Deleted {
		t = "edited  " + t
	}
	text := glib.MarkupEscapeText(t)
	if m.FromMe {
		text += "  " + map[int]string{
			store.StatusPending: "🕓", store.StatusSent: "✓", store.StatusDelivered: "✓✓",
			store.StatusRead: `<span foreground="#34b7f1">✓✓</span>`, store.StatusFailed: "⚠ not sent",
		}[m.Status]
	}
	l := gtk.NewLabel("")
	l.SetMarkup(text)
	l.SetXAlign(1)
	l.AddCSSClass("meta")
	return l
}

func (c *Conversation) reactions(m *store.Message) *gtk.Label {
	list, _ := c.w.c.Store.Reactions(m.Chat, m.ID)
	if len(list) == 0 {
		return nil
	}
	count := map[string]int{}
	var order, who []string
	for _, r := range list {
		if count[r.Emoji] == 0 {
			order = append(order, r.Emoji)
		}
		count[r.Emoji]++
		who = append(who, c.w.c.Name(r.Sender)+" "+r.Emoji)
	}
	var parts []string
	for _, e := range order {
		if count[e] > 1 {
			e += fmt.Sprint(count[e])
		}
		parts = append(parts, e)
	}
	l := gtk.NewLabel(strings.Join(parts, " "))
	l.AddCSSClass("reactions")
	l.SetTooltipText(strings.Join(who, "\n"))
	l.SetMarginTop(-4)
	return l
}

// texture loads an image file scaled down for display, off the UI thread.
func texture(path string, max int, done func(*gdk.MemoryTexture)) {
	go func() {
		img, err := media.Scaled(path, max)
		if err != nil {
			return
		}
		glib.IdleAdd(func() { done(memoryTexture(img)) })
	}()
}

func memoryTexture(img *image.NRGBA) *gdk.MemoryTexture {
	b := img.Bounds()
	return gdk.NewMemoryTexture(b.Dx(), b.Dy(), gdk.MemoryR8G8B8A8, glib.NewBytes(img.Pix), uint(img.Stride))
}

func fitted(w, h, max int) (int, int) {
	if w <= 0 || h <= 0 {
		return max, max * 3 / 4
	}
	if w >= h {
		return max, max * h / w
	}
	return max * w / h, max
}

// picture shows a photo or sticker: the embedded preview first, the real
// file once downloaded. Clicking opens it in the image viewer.
func (c *Conversation) picture(m *store.Message, max int) gtk.Widgetter {
	pic := gtk.NewPicture()
	pic.SetContentFit(gtk.ContentFitContain)
	pic.SetCanShrink(true)
	pic.SetHAlign(gtk.AlignStart)
	if m.FromMe {
		pic.SetHAlign(gtk.AlignEnd)
	}
	pic.SetSizeRequest(max, max*3/4)
	if m.Kind == store.KindSticker {
		pic.SetSizeRequest(max, max)
	}
	show := func(tex gdk.Paintabler, w, h int) {
		pic.SetPaintable(tex)
		if m.Kind != store.KindSticker {
			pic.SetSizeRequest(fitted(w, h, max))
		}
	}
	if len(m.Thumb) > 0 {
		if tex, err := gdk.NewTextureFromBytes(glib.NewBytes(m.Thumb)); err == nil {
			show(tex, tex.Width(), tex.Height())
		}
	}
	c.w.c.Fetch(m, func(path string, err error) {
		if err != nil {
			return
		}
		texture(path, max*2, func(tex *gdk.MemoryTexture) { show(tex, tex.Width(), tex.Height()) })
	})
	click := gtk.NewGestureClick()
	click.ConnectReleased(func(int, float64, float64) {
		if m.Kind == store.KindImage {
			c.view(m)
		}
	})
	pic.AddController(click)
	return pic
}

func (c *Conversation) video(m *store.Message) gtk.Widgetter {
	frame := gtk.NewOverlay()
	pic := gtk.NewPicture()
	pic.SetContentFit(gtk.ContentFitContain)
	pic.SetSizeRequest(320, 180)
	if tex, err := gdk.NewTextureFromBytes(glib.NewBytes(m.Thumb)); err == nil && len(m.Thumb) > 0 {
		pic.SetPaintable(tex)
		pic.SetSizeRequest(fitted(tex.Width(), tex.Height(), 320))
	}
	frame.SetChild(pic)
	play := gtk.NewButtonFromIconName("media-playback-start-symbolic")
	play.AddCSSClass("osd")
	play.AddCSSClass("circular")
	play.SetHAlign(gtk.AlignCenter)
	play.SetVAlign(gtk.AlignCenter)
	frame.AddOverlay(play)

	start := func(gif bool) {
		play.SetSensitive(false)
		c.w.c.Fetch(m, func(path string, err error) {
			glib.IdleAdd(func() {
				if err != nil {
					play.SetSensitive(true)
					c.w.toast("Could not download the video")
					return
				}
				v := gtk.NewVideoForFilename(path)
				if p := pic.Paintable(); p != nil {
					v.SetSizeRequest(fitted(p.IntrinsicWidth(), p.IntrinsicHeight(), 320))
				} else {
					v.SetSizeRequest(320, 180)
				}
				v.SetAutoplay(true)
				v.SetLoop(gif)
				frame.SetChild(v)
				frame.RemoveOverlay(play)
			})
		})
	}
	play.ConnectClicked(func() { c.view(m) })
	if m.Animated { // GIFs play by themselves, like in WhatsApp
		start(true)
	}
	return frame
}

func (c *Conversation) audio(m *store.Message) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationHorizontal, 6)
	icon := "🎵"
	if m.Voice {
		icon = "🎤"
	}
	box.Append(gtk.NewLabel(icon))
	wait := gtk.NewSpinner()
	wait.Start()
	box.Append(wait)
	c.w.c.Fetch(m, func(path string, err error) {
		glib.IdleAdd(func() {
			box.Remove(wait)
			if err != nil {
				box.Append(italic("Could not download"))
				return
			}
			controls := gtk.NewMediaControls(gtk.NewMediaFileForFilename(path))
			controls.SetSizeRequest(260, -1)
			box.Append(controls)
		})
	})
	return box
}

func (c *Conversation) document(m *store.Message) gtk.Widgetter {
	btn := gtk.NewButton()
	row := gtk.NewBox(gtk.OrientationHorizontal, 8)
	row.Append(gtk.NewImageFromIconName("text-x-generic-symbolic"))
	name := gtk.NewLabel(m.FileName)
	name.SetEllipsize(pango.EllipsizeMiddle)
	name.SetMaxWidthChars(40)
	row.Append(name)
	btn.SetChild(row)
	btn.SetTooltipText("Open")
	btn.ConnectClicked(func() { c.openFile(m) })
	return btn
}

func (c *Conversation) poll(m *store.Message) gtk.Widgetter {
	box := gtk.NewBox(gtk.OrientationVertical, 4)
	q := gtk.NewLabel("📊 " + m.Text)
	q.SetXAlign(0)
	q.SetWrap(true)
	q.AddCSSClass("heading")
	box.Append(q)
	votes, _ := c.w.c.Store.Votes(m.Chat, m.ID)
	count := map[string]int{}
	mine := map[string]bool{}
	for voter, opts := range votes {
		for _, o := range opts {
			count[o]++
			if voter == c.w.c.Me().String() {
				mine[o] = true
			}
		}
	}
	var buttons []*gtk.CheckButton
	var first *gtk.CheckButton
	for _, opt := range m.Options {
		b := gtk.NewCheckButtonWithLabel(fmt.Sprintf("%s  (%d)", opt, count[opt]))
		if !m.MultiVote {
			if first == nil {
				first = b
			} else {
				b.SetGroup(first)
			}
		}
		b.SetActive(mine[opt])
		buttons = append(buttons, b)
		box.Append(b)
	}
	for _, b := range buttons {
		b.ConnectToggled(func() {
			if !m.MultiVote && !b.Active() {
				return // the radio being switched off; its partner's toggle votes
			}
			var chosen []string
			for i, x := range buttons {
				if x.Active() {
					chosen = append(chosen, m.Options[i])
				}
			}
			if err := c.w.c.Vote(m, chosen); err != nil {
				c.w.toast("Vote failed: " + err.Error())
			}
		})
	}
	return box
}

// openFile downloads if needed and opens the file in its default app.
func (c *Conversation) openFile(m *store.Message) {
	c.w.c.Fetch(m, func(path string, err error) {
		glib.IdleAdd(func() {
			if err != nil {
				c.w.toast("Could not download: " + err.Error())
				return
			}
			gtk.NewFileLauncher(gio.NewFileForPath(path)).Launch(context.Background(), &c.w.win.Window, nil)
		})
	})
}

// save copies a message's file into ~/Downloads under its own name.
func (c *Conversation) save(m *store.Message) {
	c.w.c.Fetch(m, func(path string, err error) {
		glib.IdleAdd(func() {
			if err != nil {
				c.w.toast("Could not download: " + err.Error())
				return
			}
			dir := glib.GetUserSpecialDir(glib.UserDirectoryDownload)
			if dir == "" {
				dir = filepath.Join(os.Getenv("HOME"), "Downloads")
			}
			name := m.FileName
			if name == "" {
				name = "WhatsApp " + m.Time.Format("2006-01-02 15.04.05") + media.Ext(m.Mime, "")
			}
			dst := media.Unique(dir, name)
			if err := media.Copy(path, dst); err != nil {
				c.w.toast("Could not save: " + err.Error())
				return
			}
			c.w.toast("Saved to " + dst)
		})
	})
}

func (c *Conversation) cancelCompose() bool { return c.composer.cancel() }
