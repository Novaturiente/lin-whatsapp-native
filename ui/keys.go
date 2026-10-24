package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"lin-whatsapp-native/store"
)

// Keyboard use, terminal style: the chat list, the messages and the
// composer are three panes; Alt+1/2/3 or F6 move between them, and the
// messages pane has a cursor moved with the arrows or j/k.

const shortcutsHelp = `Panes
  Alt+1 / Alt+2 / Alt+3   chat list / messages / message box
  F6, Shift+F6            next / previous pane
  Alt+↑ / Alt+↓           previous / next chat
  Ctrl+K                  search chats (↓ then Enter opens)

Chat list
  ↑ ↓ or k j, g G         move          Enter, o or l   open
  /                       search chats
  Ctrl+Alt+N              new chat
  Ctrl+F or / (messages)  search in this chat (Enter older, Shift+Enter newer)

Messages pane
  ↑ ↓ or k j              move between messages
  PgUp PgDn, g G          page, first / last
  Enter or o              open photo, video or file; jump to quoted message
  r                       reply          e   edit yours
  c                       copy text      s   save file
  f                       forward        x   delete yours for everyone
  m or Menu               all actions, reactions included
  i or Esc                back to the message box

Message box
  Enter / Shift+Enter     send / new line
  ↑ in an empty box       to the messages
  Esc                     cancel reply or edit, then to the messages

Viewer
  ← → or h l, f zoom, d download, o open, c copy, r reply, space play, q close

Ctrl+Q quit · F1 or Ctrl+? this help`

func (w *Window) showHelp() {
	d := adw.NewAlertDialog("Keyboard shortcuts", "")
	l := gtk.NewLabel(shortcutsHelp)
	l.AddCSSClass("monospace")
	l.SetXAlign(0)
	l.SetSelectable(true)
	d.SetExtraChild(l)
	d.AddResponse("close", "Close")
	d.Present(w.win)
}

// windowKey handles the shortcuts that work from anywhere.
func (w *Window) windowKey(keyval uint, state gdk.ModifierType) bool {
	ctrl, alt, shift := state&gdk.ControlMask != 0, state&gdk.AltMask != 0, state&gdk.ShiftMask != 0
	if w.win.VisibleDialog() != nil { // a dialog owns the keys while it is up
		return false
	}
	switch {
	case ctrl && (keyval == gdk.KEY_q || keyval == gdk.KEY_w):
		w.win.Close()
	case ctrl && keyval == gdk.KEY_k:
		w.focusChats(true)
	case ctrl && !alt && keyval == gdk.KEY_f:
		w.conv.openSearch()
	case ctrl && alt && (keyval == gdk.KEY_n || keyval == gdk.KEY_N):
		w.newChat()
	case alt && keyval == gdk.KEY_1:
		w.focusChats(false)
	case alt && keyval == gdk.KEY_2:
		w.conv.focusMessages()
	case alt && keyval == gdk.KEY_3:
		w.conv.composer.focus()
	case alt && keyval == gdk.KEY_Up:
		w.stepChat(-1)
	case alt && keyval == gdk.KEY_Down:
		w.stepChat(1)
	case keyval == gdk.KEY_F6 && !shift, keyval == gdk.KEY_ISO_Left_Tab && ctrl:
		w.cyclePane(1)
	case keyval == gdk.KEY_F6 && shift:
		w.cyclePane(-1)
	case keyval == gdk.KEY_F1, ctrl && keyval == gdk.KEY_question:
		w.showHelp()
	default:
		return false
	}
	return true
}

// pane is 0 for the chat list, 1 for messages, 2 for the message box.
func (w *Window) pane() int {
	f := w.win.Focus()
	if f == nil {
		return -1
	}
	p := gtk.BaseWidget(f)
	switch {
	case p.IsAncestor(w.chats.root):
		return 0
	case p.IsAncestor(w.conv.composer.root):
		return 2
	case p == gtk.BaseWidget(w.conv.scroll) || p.IsAncestor(w.conv.scroll):
		return 1
	}
	return -1
}

func (w *Window) cyclePane(d int) {
	switch (w.pane() + d + 3) % 3 {
	case 0:
		w.focusChats(false)
	case 1:
		w.conv.focusMessages()
	default:
		w.conv.composer.focus()
	}
}

// focusChats puts focus on the open chat's row, or on the search box.
func (w *Window) focusChats(search bool) {
	w.split.SetShowContent(false)
	if search {
		w.chats.search.GrabFocus()
		return
	}
	if r := w.chats.rows[w.conv.chat]; r != nil {
		r.row.GrabFocus()
	} else if row := w.chats.list.RowAtIndex(0); row != nil {
		row.GrabFocus()
	}
}

// stepChat opens the chat above or below the open one in the list.
func (w *Window) stepChat(d int) {
	r := w.chats.rows[w.conv.chat]
	if r == nil {
		if row := w.chats.list.RowAtIndex(0); row != nil {
			w.openChat(row.Name())
		}
		return
	}
	for i := r.row.Index() + d; i >= 0; i += d {
		row := w.chats.list.RowAtIndex(i)
		if row == nil {
			return
		}
		if row.ChildVisible() { // skip rows the search hides
			w.openChat(row.Name())
			return
		}
	}
}

// --- messages pane --------------------------------------------------------

// focusMessages moves focus to the messages and puts the cursor on the
// newest one if it has none. It reports whether there was a chat to focus.
func (c *Conversation) focusMessages() bool {
	if c.chat == "" {
		return false
	}
	c.w.split.SetShowContent(true)
	c.scroll.GrabFocus()
	if c.cursor == nil || c.cursor.Parent() == nil {
		c.moveCursor(c.lastSlot())
	}
	return true
}

func (c *Conversation) lastSlot() *gtk.Box {
	for w := c.box.LastChild(); w != nil; w = gtk.BaseWidget(w).PrevSibling() {
		if s := c.slots[gtk.BaseWidget(w).Name()]; s != nil {
			return s
		}
	}
	return nil
}

func (c *Conversation) firstSlot() *gtk.Box {
	for w := c.box.FirstChild(); w != nil; w = gtk.BaseWidget(w).NextSibling() {
		if s := c.slots[gtk.BaseWidget(w).Name()]; s != nil {
			return s
		}
	}
	return nil
}

// sibling is the message slot d steps from the cursor, skipping notes.
func (c *Conversation) sibling(d int) *gtk.Box {
	if c.cursor == nil {
		return c.lastSlot()
	}
	w := gtk.Widgetter(c.cursor)
	for {
		if d < 0 {
			w = gtk.BaseWidget(w).PrevSibling()
		} else {
			w = gtk.BaseWidget(w).NextSibling()
		}
		if w == nil {
			return nil
		}
		if s := c.slots[gtk.BaseWidget(w).Name()]; s != nil {
			return s
		}
	}
}

func (c *Conversation) moveCursor(s *gtk.Box) {
	if s == nil {
		return
	}
	if c.cursor != nil {
		c.cursor.RemoveCSSClass("cursor")
	}
	c.cursor = s
	s.AddCSSClass("cursor")
	glib.IdleAdd(func() { c.reveal(s) })
}

// reveal scrolls just enough to show a message, like a terminal pager.
func (c *Conversation) reveal(s *gtk.Box) {
	r, ok := s.ComputeBounds(c.box)
	if !ok {
		return
	}
	a := c.scroll.VAdjustment()
	top, bottom := float64(r.Y()), float64(r.Y()+r.Height())
	switch {
	case top < a.Value():
		a.SetValue(top - 12)
	case bottom > a.Value()+a.PageSize():
		a.SetValue(bottom - a.PageSize() + 12)
	}
}

func (c *Conversation) cursorMessage() *store.Message {
	if c.cursor == nil {
		return nil
	}
	m, _ := c.w.c.Store.Message(c.chat, c.cursor.Name())
	return m
}

// messagesKey is the messages pane's own keys.
func (c *Conversation) messagesKey(keyval uint, state gdk.ModifierType) bool {
	// Only the pane itself takes these: not a focused button inside a bubble,
	// and not a menu popover whose keys bubble up through its anchor.
	if state&(gdk.ControlMask|gdk.AltMask) != 0 || c.menuOpen || !c.scroll.HasFocus() {
		return false
	}
	a := c.scroll.VAdjustment()
	switch keyval {
	case gdk.KEY_Up, gdk.KEY_k:
		if s := c.sibling(-1); s != nil {
			c.moveCursor(s)
		} else {
			c.loadOlder() // at the top: fetch the page above
			c.moveCursor(c.sibling(-1))
		}
	case gdk.KEY_Down, gdk.KEY_j:
		c.moveCursor(c.sibling(1))
	case gdk.KEY_Page_Up:
		a.SetValue(a.Value() - a.PageSize()*0.9)
	case gdk.KEY_Page_Down:
		a.SetValue(a.Value() + a.PageSize()*0.9)
	case gdk.KEY_Home, gdk.KEY_g:
		c.moveCursor(c.firstSlot())
	case gdk.KEY_End, gdk.KEY_G:
		c.moveCursor(c.lastSlot())
	case gdk.KEY_i, gdk.KEY_Escape:
		c.composer.focus()
	case gdk.KEY_slash:
		c.openSearch()
	default:
		return c.actOnCursor(keyval)
	}
	return true
}

// actOnCursor runs a one-letter action on the message under the cursor.
func (c *Conversation) actOnCursor(keyval uint) bool {
	m := c.cursorMessage()
	if m == nil || m.Kind == store.KindSystem {
		return false
	}
	switch keyval {
	case gdk.KEY_Return, gdk.KEY_KP_Enter, gdk.KEY_o:
		switch {
		case m.Kind == store.KindImage || m.Kind == store.KindVideo:
			c.view(m)
		case len(m.Raw) > 0:
			c.openFile(m)
		case m.QuoteID != "":
			c.jumpTo(m.QuoteID)
		}
	case gdk.KEY_r:
		c.composer.replyTo(m)
	case gdk.KEY_e:
		if m.FromMe && m.Kind == store.KindText && time.Since(m.Time) < 15*time.Minute {
			c.composer.edit(m)
		}
	case gdk.KEY_c:
		if m.Text != "" {
			c.box.Clipboard().SetText(m.Text)
			c.w.toast("Copied")
		}
	case gdk.KEY_s:
		if len(m.Raw) > 0 {
			c.save(m)
		}
	case gdk.KEY_f:
		c.forward(m)
	case gdk.KEY_x, gdk.KEY_Delete:
		if m.FromMe && !m.Deleted {
			c.confirmDelete(m)
		}
	case gdk.KEY_m, gdk.KEY_Menu:
		c.menu(c.cursor, m, 60, 12)
	default:
		return false
	}
	return true
}

// --- search in chat -------------------------------------------------------

func (c *Conversation) buildSearch() *gtk.SearchBar {
	c.searchBar = gtk.NewSearchBar()
	c.searchBar.AddCSSClass("chat-search")
	c.searchEntry = gtk.NewSearchEntry()
	c.searchEntry.SetPlaceholderText("Search this chat")
	c.searchEntry.SetHExpand(true)
	c.searchCount = gtk.NewLabel("")
	c.searchCount.AddCSSClass("meta")
	up := gtk.NewButtonFromIconName("go-up-symbolic")
	up.SetTooltipText("Older match (Enter)")
	up.ConnectClicked(func() { c.searchStep(1) })
	down := gtk.NewButtonFromIconName("go-down-symbolic")
	down.SetTooltipText("Newer match (Shift+Enter)")
	down.ConnectClicked(func() { c.searchStep(-1) })
	row := gtk.NewBox(gtk.OrientationHorizontal, 6)
	row.Append(c.searchEntry)
	row.Append(c.searchCount)
	row.Append(up)
	row.Append(down)
	c.searchBar.SetChild(row)
	c.searchBar.ConnectEntry(c.searchEntry)

	var timer glib.SourceHandle
	c.searchEntry.ConnectSearchChanged(func() {
		if timer != 0 {
			glib.SourceRemove(timer)
		}
		timer = glib.TimeoutAdd(200, func() bool { // wait for the typing to settle
			timer = 0
			c.runSearch()
			return false
		})
	})
	c.searchEntry.ConnectActivate(func() { c.searchStep(1) })
	c.searchEntry.ConnectNextMatch(func() { c.searchStep(1) })
	c.searchEntry.ConnectPreviousMatch(func() { c.searchStep(-1) })
	c.searchEntry.ConnectStopSearch(func() { c.closeSearch() })
	keys := gtk.NewEventControllerKey()
	keys.SetPropagationPhase(gtk.PhaseCapture)
	keys.ConnectKeyPressed(func(keyval, _ uint, state gdk.ModifierType) bool {
		if (keyval == gdk.KEY_Return || keyval == gdk.KEY_KP_Enter) && state&gdk.ShiftMask != 0 {
			c.searchStep(-1)
			return true
		}
		return false
	})
	c.searchEntry.AddController(keys)
	return c.searchBar
}

func (c *Conversation) openSearch() {
	if c.chat == "" {
		return
	}
	c.searchBar.SetSearchMode(true)
	c.searchEntry.GrabFocus()
	c.searchEntry.SelectRegion(0, -1)
}

func (c *Conversation) closeSearch() {
	c.searchBar.SetSearchMode(false)
	c.hits, c.hit = nil, -1
	c.searchCount.SetText("")
	c.composer.focus()
}

func (c *Conversation) runSearch() {
	q := strings.TrimSpace(c.searchEntry.Text())
	c.hits, c.hit = nil, -1
	if q == "" {
		c.searchCount.SetText("")
		return
	}
	c.hits, _ = c.w.c.Store.Search(c.chat, q, 500)
	if len(c.hits) == 0 {
		c.searchCount.SetText("No results")
		return
	}
	c.searchStep(1)
}

// searchStep moves to the next older (1) or newer (-1) match and lights it.
func (c *Conversation) searchStep(d int) {
	if len(c.hits) == 0 {
		return
	}
	c.hit = min(max(c.hit+d, 0), len(c.hits)-1)
	c.searchCount.SetText(fmt.Sprintf("%d of %d", c.hit+1, len(c.hits)))
	id := c.hits[c.hit].ID
	if c.jumpTo(id) {
		c.moveCursor(c.slots[id])
	}
}
