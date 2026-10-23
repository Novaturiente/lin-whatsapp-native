package ui

import (
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
	// by row index: gotk4 gives each signal a new Go wrapper, so row
	// pointers never match a map key
	jids    []string
	names   []string
	pending bool
	avatars map[string]*gdk.Texture // chat JID to picture; nil while unknown
	asked   map[string]bool
	sem     chan struct{} // profile picture downloads in flight
}

func newChatList(w *Window) *ChatList {
	l := &ChatList{w: w, root: adw.NewToolbarView(), search: gtk.NewSearchEntry(), list: gtk.NewListBox(),
		avatars: map[string]*gdk.Texture{}, asked: map[string]bool{}, sem: make(chan struct{}, 3)}
	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("WhatsApp", ""))
	l.root.AddTopBar(header)

	l.search.SetPlaceholderText("Search chats")
	l.search.SetMarginStart(8)
	l.search.SetMarginEnd(8)
	l.search.SetMarginBottom(6)
	l.search.ConnectSearchChanged(l.list.InvalidateFilter)
	l.list.SetFilterFunc(func(row *gtk.ListBoxRow) bool {
		q := strings.ToLower(l.search.Text())
		return q == "" || strings.Contains(strings.ToLower(at(l.names, row)), q)
	})
	l.list.AddCSSClass("navigation-sidebar")
	l.list.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if jid := at(l.jids, row); jid != "" {
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

func (l *ChatList) rebuild() {
	chats, err := l.w.c.Store.Chats()
	if err != nil {
		return
	}
	l.list.RemoveAll()
	l.jids, l.names = nil, nil
	for _, c := range chats {
		name := l.w.c.Name(c.JID)
		row := gtk.NewListBoxRow()
		row.SetChild(l.rowContent(c, name))
		l.jids, l.names = append(l.jids, c.JID), append(l.names, name)
		l.list.Append(row)
		if c.JID == l.w.conv.chat {
			l.list.SelectRow(row)
		}
	}
}

func (l *ChatList) rowContent(c store.Chat, name string) gtk.Widgetter {
	avatar := adw.NewAvatar(42, name, true)
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
	if c.Unread > 0 {
		badge := gtk.NewLabel(strconv.Itoa(c.Unread))
		badge.AddCSSClass("unread")
		bottom.Append(badge)
	}
	text := gtk.NewBox(gtk.OrientationVertical, 2)
	text.SetVAlign(gtk.AlignCenter)
	text.Append(top)
	text.Append(bottom)
	row := gtk.NewBox(gtk.OrientationHorizontal, 10)
	row.SetMarginTop(4)
	row.SetMarginBottom(4)
	row.Append(avatar)
	row.Append(text)
	return row
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
				l.reload()
				l.w.conv.refreshTitle()
			}
		})
	}()
	return nil
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
