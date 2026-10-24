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
	w          *Window
	root       *adw.ToolbarView
	title      *adw.WindowTitle
	avatar     *adw.Avatar
	stack      *gtk.Stack
	scroll     *gtk.ScrolledWindow
	box        *gtk.Box
	chat       string
	slots      map[string]*gtk.Box // message ID to the row holding its bubble
	oldest     *store.Message
	loading    bool
	storeEnd   bool         // every stored message is shown; older ones are on the phone
	asking     *gtk.Label   // "Loading older messages…" while the phone is asked
	askGen     int          // which request a timeout belongs to
	cursor     *gtk.Box     // the message the keyboard acts on
	lastSender string       // sender of the newest bubble shown, for avatar runs
	menuOpen   bool         // a message menu is up; the pane's letter keys stand down
	selLabel   *gtk.Label   // last message text pressed; Ctrl+C copies its selection
	clamps     []*adw.Clamp // every shown bubble's width cap, resized with the pane
	bubbleMax  int          // that cap: Settings.BubbleWidth % of the pane's width

	searchBar   *gtk.SearchBar
	searchEntry *gtk.SearchEntry
	searchCount *gtk.Label
	hits        []*store.Message // search matches, newest first
	hit         int
	phoneEnd    bool // the phone answered with nothing older
	composer    *Composer
	typers      map[string]time.Time
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
	c.root.AddTopBar(c.buildSearch())
	find := gtk.NewButtonFromIconName("system-search-symbolic")
	find.SetTooltipText("Search messages (Ctrl+F or /)")
	find.ConnectClicked(c.openSearch)
	header.PackEnd(find)

	c.box.SetMarginStart(12)
	c.box.SetMarginEnd(12)
	c.box.SetMarginTop(8)
	c.box.SetMarginBottom(8)
	c.box.SetVAlign(gtk.AlignEnd)
	c.scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	c.scroll.SetChild(c.box)
	c.scroll.SetVExpand(true)
	c.scroll.AddCSSClass("conversation")
	c.scroll.SetFocusable(true) // the messages pane takes arrow keys; see messagesKey
	msgKeys := gtk.NewEventControllerKey()
	msgKeys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		return c.messagesKey(keyval, state)
	})
	c.scroll.AddController(msgKeys)
	// GTK has no percentage widths: follow the pane's width through its
	// horizontal adjustment, whose page size is the visible width.
	c.bubbleMax = 560
	c.scroll.HAdjustment().ConnectChanged(c.resizeBubbles)
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
	c.chat, c.oldest, c.typers, c.storeEnd, c.asking, c.loading = chat, nil, map[string]time.Time{}, false, nil, false
	c.phoneEnd = false
	c.askGen++
	c.cursor, c.hits, c.hit = nil, nil, -1
	c.searchBar.SetSearchMode(false)
	for child := c.box.FirstChild(); child != nil; child = c.box.FirstChild() {
		c.box.Remove(child)
	}
	c.slots = map[string]*gtk.Box{}
	c.clamps = nil
	c.stack.SetVisibleChildName("chat")
	c.composer.root.SetVisible(true)
	c.composer.reset()
	c.refreshTitle()
	page, _ := c.w.c.Store.Messages(chat, nil, pageSize)
	c.w.chats.waiters = map[string][]*adw.Avatar{} // the old chat's bubbles are gone
	c.lastSender = ""
	for _, m := range page {
		c.box.Append(c.slot(m, m.Sender != c.lastSender))
		c.lastSender = sender(m)
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
	if !c.loadOlderPage() {
		c.askPhone()
	}
}

// loadOlderPage shows the next older page from the store; false when the
// store has no more.
func (c *Conversation) loadOlderPage() bool {
	if c.oldest == nil {
		return false
	}
	page, _ := c.w.c.Store.Messages(c.chat, c.oldest, pageSize)
	if len(page) == 0 {
		c.storeEnd = true
		return false
	}
	c.loading = true
	a := c.scroll.VAdjustment()
	upper := a.Upper()
	for i := len(page) - 1; i >= 0; i-- {
		c.box.Prepend(c.slot(page[i], i == 0 || sender(page[i-1]) != page[i].Sender))
	}
	c.oldest = page[0]
	glib.IdleAdd(func() { // keep the message under the reader where it was
		a.SetValue(a.Value() + a.Upper() - upper)
		c.loading = false
	})
	return true
}

// askPhone requests messages older than everything stored, as WhatsApp Web
// does when you scroll past its cache. The answer arrives as EvHistory.
func (c *Conversation) askPhone() {
	if c.loading || c.phoneEnd {
		return
	}
	anchor, _ := c.w.c.Store.OldestReal(c.chat)
	if anchor == nil {
		return // nothing the phone could page back from
	}
	c.note("Loading older messages from your phone…")
	c.loading = true
	c.askGen++
	gen := c.askGen
	c.w.c.RequestOlder(anchor)
	glib.TimeoutSecondsAdd(20, func() bool { // the phone may never answer
		if gen == c.askGen && c.loading {
			c.note("Your phone sent no older messages. Scroll up to try again.")
			c.loading = false
		}
		return false
	})
}

// note shows one line above the oldest message, replacing the last one.
func (c *Conversation) note(text string) {
	if c.asking != nil {
		c.box.Remove(c.asking)
	}
	c.asking = italic(text)
	c.asking.SetHAlign(gtk.AlignCenter)
	c.box.Prepend(c.asking)
}

func (c *Conversation) historyArrived() {
	if !c.loading {
		return
	}
	c.askGen++ // the timeout no longer applies
	c.loading = false
	if older, _ := c.w.c.Store.Messages(c.chat, c.oldest, 1); len(older) == 0 {
		c.phoneEnd = true // asking again would bring the same nothing
		c.note("No older messages on your phone.")
		return
	}
	if c.asking != nil {
		c.box.Remove(c.asking)
		c.asking = nil
	}
	c.storeEnd = false
	c.loadOlder()
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
	c.box.Append(c.slot(m, m.Sender != c.lastSender))
	c.lastSender = sender(m)
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
	if old := slot.LastChild(); old != nil { // the bubble; an avatar column stays
		slot.Remove(old)
	}
	slot.Append(c.bubble(m))
}

// resizeBubbles caps bubbles at the Settings share of the pane's width.
func (c *Conversation) resizeBubbles() {
	max := int(c.scroll.HAdjustment().PageSize() * float64(c.w.set.BubbleWidth) / 100)
	if max < 200 || max == c.bubbleMax {
		return
	}
	c.bubbleMax = max
	for _, cl := range c.clamps {
		cl.SetMaximumSize(max)
		cl.SetTighteningThreshold(max)
	}
}

// sender is who a run of bubbles belongs to; notice lines break runs.
func sender(m *store.Message) string {
	if m.Kind == store.KindSystem {
		return ""
	}
	return m.Sender
}

// slot holds one message. In groups, others' messages get the sender's
// picture beside the first bubble of each run, as WhatsApp does.
func (c *Conversation) slot(m *store.Message, runStart bool) *gtk.Box {
	slot := gtk.NewBox(gtk.OrientationHorizontal, 6)
	slot.AddCSSClass("slot")
	slot.SetName(m.ID) // gotk4 wrappers differ per call; the name is the key
	if isGroup(m.Chat) && !m.FromMe && m.Kind != store.KindSystem {
		if runStart {
			av := adw.NewAvatar(30, c.w.c.Name(m.Sender), true)
			av.SetVAlign(gtk.AlignStart)
			av.SetMarginTop(2)
			c.w.chats.attach(m.Sender, av)
			slot.Append(av)
		} else {
			gap := gtk.NewBox(gtk.OrientationHorizontal, 0)
			gap.SetSizeRequest(30, -1)
			slot.Append(gap)
		}
	}
	slot.Append(c.bubble(m))
	c.slots[m.ID] = slot
	return slot
}

// bubble draws one message.
func (c *Conversation) bubble(m *store.Message) gtk.Widgetter {
	if m.Kind == store.KindSystem {
		l := gtk.NewLabel(m.Text)
		l.SetWrap(true)
		l.SetJustify(gtk.JustifyCenter)
		l.AddCSSClass("system")
		l.SetHAlign(gtk.AlignCenter)
		l.SetHExpand(true)
		l.SetTooltipText(m.Time.Format("2 Jan 2006, 15:04"))
		return l
	}
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
	if m.Forwarded && !m.Deleted {
		f := gtk.NewLabel("↪ Forwarded")
		f.SetXAlign(0)
		f.AddCSSClass("meta")
		col.Append(f)
	}
	if m.QuoteID != "" && !m.Deleted {
		col.Append(c.quote(m))
	}
	c.content(col, m)
	col.Append(c.meta(m))

	// A bubble grows with its text up to Settings.BubbleWidth % of the pane, then wraps.
	clamp := adw.NewClamp()
	clamp.SetMaximumSize(c.bubbleMax)
	clamp.SetTighteningThreshold(c.bubbleMax)
	c.clamps = append(c.clamps, clamp) // shortcut: edited bubbles leave stale entries until the chat reloads
	clamp.SetChild(col)
	clamp.SetHAlign(col.HAlign())
	outer := gtk.NewBox(gtk.OrientationVertical, 0)
	outer.SetHExpand(true)
	outer.Append(clamp)
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
	l.SetCanFocus(false)   // selectable labels grab focus and select all
	press := gtk.NewGestureClick() // unfocused labels miss their own Ctrl+C; see copySelection
	press.SetPropagationPhase(gtk.PhaseCapture)
	press.ConnectPressed(func(int, float64, float64) { c.selLabel = l })
	l.AddController(press)
	l.SetMaxWidthChars(-1) // natural width is the whole line; the bubble's clamp caps it
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
	if m.LinkTitle != "" {
		col.Append(c.linkCard(m))
	}
	if m.Text != "" {
		col.Append(c.textLabel(m))
	}
}

// linkCard is the preview the sender's phone attached to a link.
func (c *Conversation) linkCard(m *store.Message) gtk.Widgetter {
	card := gtk.NewBox(gtk.OrientationHorizontal, 8)
	card.AddCSSClass("quote")
	if len(m.Thumb) > 0 {
		if tex, err := gdk.NewTextureFromBytes(glib.NewBytes(m.Thumb)); err == nil {
			pic := gtk.NewPictureForPaintable(tex)
			pic.SetSizeRequest(64, 64)
			pic.SetContentFit(gtk.ContentFitCover)
			card.Append(pic)
		}
	}
	text := gtk.NewBox(gtk.OrientationVertical, 2)
	title := gtk.NewLabel(m.LinkTitle)
	title.SetXAlign(0)
	title.SetWrap(true)
	title.AddCSSClass("heading")
	text.Append(title)
	if m.LinkDesc != "" {
		desc := gtk.NewLabel(m.LinkDesc)
		desc.SetXAlign(0)
		desc.SetWrap(true)
		desc.SetLines(3)
		desc.SetEllipsize(pango.EllipsizeEnd)
		desc.AddCSSClass("dim-label")
		text.Append(desc)
	}
	if m.LinkURL != "" {
		u := gtk.NewLabel(m.LinkURL)
		u.SetXAlign(0)
		u.SetEllipsize(pango.EllipsizeEnd)
		u.AddCSSClass("meta")
		text.Append(u)
	}
	card.Append(text)
	click := gtk.NewGestureClick()
	click.ConnectReleased(func(int, float64, float64) {
		if m.LinkURL == "" {
			return
		}
		url := m.LinkURL
		if !strings.Contains(url, "://") {
			url = "https://" + url
		}
		gtk.NewURILauncher(url).Launch(context.Background(), &c.w.win.Window, nil)
	})
	card.AddController(click)
	return card
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
	click.ConnectReleased(func(int, float64, float64) {
		if !c.jumpTo(m.QuoteID) {
			c.w.toast("That message is not on this computer")
		}
	})
	box.AddController(click)
	return box
}

// jumpTo scrolls to a loaded message, loading older pages until it appears.
// It reports whether the message was found.
func (c *Conversation) jumpTo(id string) bool {
	if m, _ := c.w.c.Store.Message(c.chat, id); m == nil {
		return false // never stored: paging back would build the whole history for nothing
	}
	for pages := 0; ; pages++ {
		if slot, ok := c.slots[id]; ok {
			glib.TimeoutAdd(60, func() bool { // after the pages just added have a size
				c.centre(slot)
				c.flash(slot)
				return false
			})
			return true
		}
		if pages == 50 { // 3000 messages back; more would freeze the window
			c.w.toast("That message is too far back to show here")
			return true
		}
		if !c.loadOlderPage() {
			return false
		}
	}
}

// centre scrolls a message to the middle of the view.
func (c *Conversation) centre(w gtk.Widgetter) {
	if r, ok := gtk.BaseWidget(w).ComputeBounds(c.box); ok {
		a := c.scroll.VAdjustment()
		a.SetValue(float64(r.Y()) + float64(r.Height())/2 - a.PageSize()/2)
	}
}

// flash tints a message's row for a moment, so the eye finds it.
func (c *Conversation) flash(slot *gtk.Box) {
	slot.AddCSSClass("flash")
	glib.TimeoutAdd(1600, func() bool {
		slot.RemoveCSSClass("flash")
		return false
	})
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
	show := func(jpg []byte) {
		if tex, err := gdk.NewTextureFromBytes(glib.NewBytes(jpg)); err == nil {
			pic.SetPaintable(tex)
			pic.SetSizeRequest(fitted(tex.Width(), tex.Height(), 320))
		}
	}
	if len(m.Thumb) > 0 {
		show(m.Thumb)
	} else if size := media.Size(m.Raw); size > 0 && size <= 25<<20 && !m.Animated {
		// Sent without a preview (WhatsApp Web and desktop do this): make one
		// from the first frame, once, and keep it.
		c.w.c.Fetch(m, func(path string, err error) {
			if err != nil {
				return
			}
			go func() {
				jpg := media.VideoThumbnail(path)
				if len(jpg) == 0 {
					return
				}
				_ = c.w.c.Store.SetThumb(m.Chat, m.ID, jpg)
				glib.IdleAdd(func() { show(jpg) })
			}()
		})
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
