package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"lin-whatsapp-native/store"
)

type ChatList struct {
	w      *Window
	root   *adw.ToolbarView
	search *gtk.SearchEntry
	list   *gtk.ListBox
	// Rows are found by their GTK widget name, which holds the chat JID:
	// gotk4 hands each signal a new Go wrapper, so row pointers never match.
	rows       map[string]*chatRow
	pending    bool
	title      *adw.WindowTitle
	archiveBtn *gtk.ToggleButton       // shows archived chats instead of the rest
	avatars    map[string]*gdk.Texture // chat JID to picture; nil while unknown
	asked      map[string]bool
	sem        chan struct{}            // profile picture downloads in flight
	waiters    map[string][]*adw.Avatar // avatars elsewhere waiting for a picture
}

// chatRow is one chat's row and what it was last drawn from, so a refresh
// redraws only the rows that changed.
type chatRow struct {
	row    *gtk.ListBoxRow
	chat   store.Chat
	muted  bool // as drawn: a mute ends with the clock, not with a store change
	name   string
	avatar bool
}

func newChatList(w *Window) *ChatList {
	l := &ChatList{w: w, root: adw.NewToolbarView(), search: gtk.NewSearchEntry(), list: gtk.NewListBox(),
		rows: map[string]*chatRow{}, avatars: map[string]*gdk.Texture{}, asked: map[string]bool{},
		waiters: map[string][]*adw.Avatar{},
		sem:     make(chan struct{}, 3)}
	header := adw.NewHeaderBar()
	l.title = adw.NewWindowTitle("WhatsApp", "")
	header.SetTitleWidget(l.title)
	l.archiveBtn = gtk.NewToggleButton()
	l.archiveBtn.SetIconName("folder-symbolic")
	l.archiveBtn.SetTooltipText("Archived chats")
	l.archiveBtn.SetVisible(false)
	l.archiveBtn.ConnectToggled(func() {
		if l.archiveBtn.Active() {
			l.title.SetTitle("Archived")
		} else {
			l.title.SetTitle("WhatsApp")
		}
		l.list.InvalidateFilter()
	})
	starred := gtk.NewButtonFromIconName("starred-symbolic")
	starred.SetTooltipText("Starred messages")
	starred.ConnectClicked(w.showStarred)
	newChat := gtk.NewButtonFromIconName("document-edit-symbolic")
	newChat.SetTooltipText("New chat (Ctrl+Alt+N)")
	newChat.ConnectClicked(w.newChat)
	prefs := gtk.NewButtonFromIconName("emblem-system-symbolic")
	prefs.SetTooltipText("Settings (Ctrl+,)")
	prefs.ConnectClicked(w.openSettings)
	header.PackStart(prefs)
	header.PackStart(starred)
	header.PackEnd(newChat)
	header.PackEnd(l.archiveBtn)
	l.root.AddTopBar(header)

	l.search.SetPlaceholderText("Search chats")
	l.search.SetMarginStart(8)
	l.search.SetMarginEnd(8)
	l.search.SetMarginBottom(6)
	l.search.ConnectSearchChanged(l.list.InvalidateFilter)
	l.search.ConnectActivate(func() { // Enter opens the first match
		if row := l.firstVisible(); row != nil {
			w.openChat(row.Name())
		}
	})
	l.search.ConnectStopSearch(func() { // Esc leaves search for the open chat
		l.search.SetText("")
		w.conv.composer.focus()
	})
	searchKeys := gtk.NewEventControllerKey()
	searchKeys.ConnectKeyPressed(func(keyval, _ uint, _ gdk.ModifierType) bool {
		if keyval != gdk.KEY_Down {
			return false
		}
		if row := l.firstVisible(); row != nil { // into the results, like a launcher
			row.GrabFocus()
		}
		return true
	})
	l.search.AddController(searchKeys)

	listKeys := gtk.NewEventControllerKey() // vim keys on top of the list's own arrows
	listKeys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if state&(gdk.ControlMask|gdk.AltMask) != 0 {
			return false
		}
		switch keyval {
		case gdk.KEY_j:
			l.stepFocus(1)
		case gdk.KEY_k:
			l.stepFocus(-1)
		case gdk.KEY_g:
			l.stepFocus(-1 << 20)
		case gdk.KEY_G:
			l.stepFocus(1 << 20)
		case gdk.KEY_l, gdk.KEY_o: // open, like Enter
			if f := l.list.FocusChild(); f != nil {
				w.openChat(gtk.BaseWidget(f).Name())
			}
		case gdk.KEY_slash:
			l.search.GrabFocus()
		case gdk.KEY_m, gdk.KEY_Menu:
			if f := l.list.FocusChild(); f != nil {
				l.menu(gtk.BaseWidget(f).Name(), 40, 20)
			}
		default:
			return false
		}
		return true
	})
	l.list.AddController(listKeys)
	l.list.SetFilterFunc(func(row *gtk.ListBoxRow) bool {
		q := strings.ToLower(l.search.Text())
		r := l.rows[row.Name()]
		if r == nil {
			return false
		}
		if q == "" { // a search looks in archived chats too
			return r.chat.Archived == l.archiveBtn.Active()
		}
		return strings.Contains(strings.ToLower(r.name), q)
	})
	l.list.SetSortFunc(func(a, b *gtk.ListBoxRow) int {
		ra, rb := l.rows[a.Name()], l.rows[b.Name()]
		if ra == nil || rb == nil {
			return 0
		}
		if ra.chat.Pinned != rb.chat.Pinned { // pinned on top, last pinned first
			return int(min(max(rb.chat.Pinned-ra.chat.Pinned, -1), 1))
		}
		return rb.chat.LastTime.Compare(ra.chat.LastTime) // newest first
	})
	l.list.AddCSSClass("navigation-sidebar")
	l.list.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if jid := row.Name(); l.rows[jid] != nil {
			w.openChat(jid)
		}
	})

	scroll := gtk.NewScrolledWindow()
	scroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroll.SetChild(l.list)
	scroll.SetVExpand(true)
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(l.search)
	box.Append(scroll)
	l.root.SetContent(box)
	return l
}

// reload rebuilds the list from the store, at most a few times a second:
// a history sync fires thousands of changes.
func (l *ChatList) reload() {
	if l.pending {
		return
	}
	l.pending = true
	glib.TimeoutAdd(250, func() bool {
		l.pending = false
		l.rebuild()
		return false
	})
}

// rebuild brings the rows in line with the store, redrawing only rows whose
// chat, name or picture changed.
func (l *ChatList) rebuild() {
	chats, err := l.w.c.Store.Chats()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	changed := false
	unread, archived := 0, 0
	for _, c := range chats {
		seen[c.JID] = true
		if !c.Muted() {
			unread += c.Unread
		}
		if c.Archived {
			archived++
		}
		name := l.w.c.Name(c.JID)
		hasAvatar := l.avatar(c.JID) != nil
		r := l.rows[c.JID]
		if r == nil {
			r = &chatRow{row: gtk.NewListBoxRow()}
			r.row.SetName(c.JID)
			jid := c.JID
			click := gtk.NewGestureClick()
			click.SetButton(gdk.BUTTON_SECONDARY)
			click.ConnectPressed(func(_ int, x, y float64) { l.menu(jid, x, y) })
			r.row.AddController(click)
			l.rows[c.JID] = r
			l.list.Append(r.row)
		} else if r.chat == c && r.name == name && r.avatar == hasAvatar && r.muted == c.Muted() {
			continue
		}
		r.chat, r.name, r.avatar, r.muted = c, name, hasAvatar, c.Muted()
		r.row.SetChild(l.rowContent(c, name))
		changed = true
	}
	for jid, r := range l.rows {
		if !seen[jid] {
			l.list.Remove(r.row)
			delete(l.rows, jid)
			changed = true
		}
	}
	if changed {
		l.list.InvalidateSort()
		l.list.InvalidateFilter()
	}
	l.archiveBtn.SetVisible(archived > 0 || l.archiveBtn.Active())
	l.archiveBtn.SetTooltipText(fmt.Sprintf("Archived chats (%d)", archived))
	title := "WhatsApp"
	if unread > 0 {
		title = fmt.Sprintf("(%d) WhatsApp", unread)
	}
	l.w.win.SetTitle(title)
	if r := l.rows[l.w.conv.chat]; r != nil {
		l.list.SelectRow(r.row)
	}
}

func (l *ChatList) rowContent(c store.Chat, name string) gtk.Widgetter {
	avatar := adw.NewAvatar(46, name, true)
	if tex := l.avatar(c.JID); tex != nil {
		avatar.SetCustomImage(tex)
	}
	title := gtk.NewLabel(name)
	title.SetXAlign(0)
	title.SetHExpand(true)
	title.SetEllipsize(pango.EllipsizeEnd)
	title.AddCSSClass("heading")
	when := gtk.NewLabel(shortTime(c.LastTime))
	when.AddCSSClass("meta")
	top := gtk.NewBox(gtk.OrientationHorizontal, 6)
	top.Append(title)
	top.Append(when)

	preview := gtk.NewLabel(firstLine(c.LastText))
	preview.SetXAlign(0)
	preview.SetHExpand(true)
	preview.SetEllipsize(pango.EllipsizeEnd)
	preview.AddCSSClass("dim-label")
	bottom := gtk.NewBox(gtk.OrientationHorizontal, 6)
	bottom.Append(preview)
	if c.Muted() {
		bottom.Append(dimIcon("audio-volume-muted-symbolic", "Muted"))
	}
	if c.Pinned != 0 {
		bottom.Append(dimIcon("view-pin-symbolic", "Pinned"))
	}
	if c.Unread > 0 {
		badge := gtk.NewLabel(strconv.Itoa(c.Unread))
		badge.AddCSSClass("unread")
		if c.Muted() {
			badge.AddCSSClass("muted")
		}
		bottom.Append(badge)
	}
	text := gtk.NewBox(gtk.OrientationVertical, 4)
	text.SetVAlign(gtk.AlignCenter)
	text.Append(top)
	text.Append(bottom)
	row := gtk.NewBox(gtk.OrientationHorizontal, 12)
	row.SetMarginTop(9)
	row.SetMarginBottom(9)
	row.SetMarginStart(4)
	row.SetMarginEnd(4)
	row.Append(avatar)
	row.Append(text)
	return row
}

func dimIcon(name, tip string) *gtk.Image {
	i := gtk.NewImageFromIconName(name)
	i.AddCSSClass("dim-label")
	i.SetTooltipText(tip)
	return i
}

// menu is the right-click popover on a chat: pin, mute, archive, info.
func (l *ChatList) menu(jid string, x, y float64) {
	r := l.rows[jid]
	if r == nil {
		return
	}
	c, wc := r.chat, l.w.c
	pop := gtk.NewPopover()
	pop.SetParent(r.row)
	at := gdk.NewRectangle(int(x), int(y), 1, 1)
	pop.SetPointingTo(&at)
	pop.ConnectClosed(func() { glib.IdleAdd(pop.Unparent) })
	col := gtk.NewBox(gtk.OrientationVertical, 2)
	pop.SetChild(col)
	act := func(label string, f func()) {
		b := gtk.NewButtonWithLabel(label)
		b.AddCSSClass("flat")
		b.Child().(*gtk.Label).SetXAlign(0)
		b.ConnectClicked(func() { pop.Popdown(); f() })
		col.Append(b)
	}
	act("Chat info", func() { l.w.chatInfo(jid) })
	pinned := 0
	for _, o := range l.rows {
		if o.chat.Pinned != 0 {
			pinned++
		}
	}
	switch {
	case c.Archived:
	case c.Pinned != 0:
		act("Unpin", func() { wc.Pin(jid, false) })
	case pinned < 3: // WhatsApp's limit
		act("Pin", func() { wc.Pin(jid, true) })
	}
	if c.Muted() {
		act("Unmute", func() { wc.Mute(jid, false, 0) })
	} else {
		act("Mute for 8 hours", func() { wc.Mute(jid, true, 8*time.Hour) })
		act("Mute for 1 week", func() { wc.Mute(jid, true, 7*24*time.Hour) })
		act("Mute always", func() { wc.Mute(jid, true, 0) })
	}
	if c.Archived {
		act("Unarchive", func() { wc.Archive(jid, false) })
	} else {
		act("Archive", func() { wc.Archive(jid, true) })
	}
	pop.Popup()
}

// avatar returns a chat's picture if it is loaded, and starts loading it
// the first time it is asked for.
func (l *ChatList) avatar(chat string) *gdk.Texture {
	if l.asked[chat] {
		return l.avatars[chat]
	}
	l.asked[chat] = true
	go func() {
		l.sem <- struct{}{}
		path := l.w.c.Avatar(chat)
		<-l.sem
		if path == "" {
			return
		}
		glib.IdleAdd(func() {
			if tex, err := gdk.NewTextureFromFilename(path); err == nil {
				l.avatars[chat] = tex
				for _, av := range l.waiters[chat] {
					av.SetCustomImage(tex)
				}
				delete(l.waiters, chat)
				l.reload()
				l.w.conv.refreshTitle()
			}
		})
	}()
	return nil
}

// attach gives an avatar widget anywhere in the window a person's picture,
// now if loaded, else when it arrives.
func (l *ChatList) attach(jid string, av *adw.Avatar) {
	if tex := l.avatar(jid); tex != nil {
		av.SetCustomImage(tex)
		return
	}
	l.waiters[jid] = append(l.waiters[jid], av)
}

// stepFocus moves keyboard focus d visible rows down (up when negative),
// stopping at either end.
func (l *ChatList) stepFocus(d int) {
	cur := 0
	if f := l.list.FocusChild(); f != nil {
		if r := l.rows[gtk.BaseWidget(f).Name()]; r != nil {
			cur = r.row.Index()
		}
	}
	step := 1
	if d < 0 {
		step, d = -1, -d
	}
	var target *gtk.ListBoxRow
	for i := cur + step; d > 0 && i >= 0; i += step {
		row := l.list.RowAtIndex(i)
		if row == nil {
			break
		}
		if row.ChildVisible() {
			target = row
			d--
		}
	}
	if target != nil {
		target.GrabFocus()
	}
}

// firstVisible is the top row the search has not hidden.
func (l *ChatList) firstVisible() *gtk.ListBoxRow {
	for i := 0; ; i++ {
		row := l.list.RowAtIndex(i)
		if row == nil || row.ChildVisible() {
			return row
		}
	}
}

// at is the entry for a row of a list built in the same order.
func at(list []string, row *gtk.ListBoxRow) string {
	if i := row.Index(); i >= 0 && i < len(list) {
		return list[i]
	}
	return ""
}

func shortTime(t time.Time) string {
	if t.Unix() <= 0 {
		return ""
	}
	now := time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon")
	}
	return t.Format("02/01/06")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
