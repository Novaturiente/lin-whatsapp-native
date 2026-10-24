package ui

import (
	"slices"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"lin-whatsapp-native/markup"
	"lin-whatsapp-native/media"
	"lin-whatsapp-native/store"
)

var quickReactions = []string{"👍", "❤️", "😂", "😮", "😢", "🙏"}

// menu is the right-click popover on a message: react, reply, copy, save,
// forward, edit and delete.
func (c *Conversation) menu(anchor *gtk.Box, m *store.Message, x, y float64) {
	if m.Deleted {
		return
	}
	pop := gtk.NewPopover()
	pop.SetParent(anchor)
	at := gdk.NewRectangle(int(x), int(y), 1, 1)
	pop.SetPointingTo(&at)
	c.menuOpen = true
	pop.ConnectClosed(func() {
		c.menuOpen = false
		glib.IdleAdd(pop.Unparent)
	})
	col := gtk.NewBox(gtk.OrientationVertical, 2)
	pop.SetChild(col)
	act := func(label string, f func()) {
		b := gtk.NewButtonWithLabel(label)
		b.AddCSSClass("flat")
		l := b.Child().(*gtk.Label)
		l.SetXAlign(0)
		l.SetMaxWidthChars(40)
		l.SetEllipsize(pango.EllipsizeMiddle)
		b.ConnectClicked(func() { pop.Popdown(); f() })
		col.Append(b)
	}

	row := gtk.NewBox(gtk.OrientationHorizontal, 0)
	for _, e := range quickReactions {
		b := gtk.NewButtonWithLabel(e)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() { pop.Popdown(); c.toggleReaction(m, e) })
		row.Append(b)
	}
	more := gtk.NewMenuButton()
	more.SetIconName("list-add-symbolic")
	more.AddCSSClass("flat")
	chooser := gtk.NewEmojiChooser()
	chooser.ConnectEmojiPicked(func(e string) { pop.Popdown(); c.toggleReaction(m, e) })
	more.SetPopover(&chooser.Popover)
	row.Append(more)
	col.Append(row)

	if m.FromMe && m.Status == store.StatusFailed {
		act("Retry sending", func() {
			if err := c.w.c.Retry(m); err != nil {
				c.w.toast(err.Error())
			}
		})
	}
	act("Reply", func() { c.composer.replyTo(m) })
	if m.Text != "" && m.Kind != store.KindPoll {
		act("Copy text", func() { anchor.Clipboard().SetText(m.Text) })
	}
	links := markup.Links(m.Text)
	if m.LinkURL != "" && !slices.Contains(links, m.LinkURL) {
		links = append(links, m.LinkURL)
	}
	for i, u := range links {
		label := "Copy link"
		if len(links) > 1 {
			label = "Copy link: " + u
		}
		if i == 3 { // a message full of links must not make the menu endless
			break
		}
		act(label, func() { anchor.Clipboard().SetText(u); c.w.toast("Link copied") })
	}
	if m.Kind == store.KindImage || m.Kind == store.KindSticker {
		act("Copy image", func() { c.copyImage(m) })
	}
	if len(m.Raw) > 0 {
		act("Save to Downloads", func() { c.save(m) })
	}
	if len(m.Raw) > 0 || (m.Text != "" && !m.ViewOnce && m.Kind != store.KindOther && m.Kind != store.KindPoll) {
		act("Forward…", func() { c.forward(m) })
	}
	if m.FromMe && m.Kind == store.KindText && time.Since(m.Time) < 15*time.Minute {
		act("Edit", func() { c.composer.edit(m) })
	}
	if m.FromMe && time.Since(m.Time) < 48*time.Hour {
		act("Delete for everyone", func() { c.confirmDelete(m) })
	}
	pop.Popup()
}

// toggleReaction sets an emoji, or clears it when it is already ours.
func (c *Conversation) toggleReaction(m *store.Message, emoji string) {
	list, _ := c.w.c.Store.Reactions(m.Chat, m.ID)
	for _, r := range list {
		if r.Sender == c.w.c.Me().String() && r.Emoji == emoji {
			emoji = ""
		}
	}
	c.w.c.React(m, emoji)
}

func (c *Conversation) copyImage(m *store.Message) {
	c.w.c.Fetch(m, func(path string, err error) {
		if err != nil {
			return
		}
		go func() { // Fetch answers inline when the file is cached; never decode on the UI thread
			img, err := media.Scaled(path, 4096)
			if err != nil {
				return
			}
			glib.IdleAdd(func() {
				c.box.Clipboard().SetTexture(memoryTexture(img))
				c.w.toast("Image copied")
			})
		}()
	})
}

func (c *Conversation) confirmDelete(m *store.Message) {
	d := adw.NewAlertDialog("Delete for everyone?", "The message is removed from this chat on every device.")
	d.AddResponse("cancel", "Cancel")
	d.AddResponse("delete", "Delete")
	d.SetResponseAppearance("delete", adw.ResponseDestructive)
	d.ConnectResponse(func(r string) {
		if r == "delete" {
			c.w.c.Revoke(m)
		}
	})
	d.Present(c.w.win)
}

// forward asks for a chat and sends the message there.
func (c *Conversation) forward(m *store.Message) {
	d := adw.NewAlertDialog("Forward to…", "")
	d.AddResponse("cancel", "Cancel")
	search := gtk.NewSearchEntry()
	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	chats, _ := c.w.c.Store.Chats()
	var jids, names []string // by row index, see ChatList.jids
	for _, ch := range chats {
		row := gtk.NewListBoxRow()
		name := c.w.c.Name(ch.JID)
		l := gtk.NewLabel(name)
		l.SetXAlign(0)
		l.SetMarginTop(6)
		l.SetMarginBottom(6)
		l.SetMarginStart(8)
		row.SetChild(l)
		jids, names = append(jids, ch.JID), append(names, name)
		list.Append(row)
	}
	list.SetFilterFunc(func(row *gtk.ListBoxRow) bool { return contains(at(names, row), search.Text()) })
	search.ConnectSearchChanged(list.InvalidateFilter)
	list.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if err := c.w.c.Forward(at(jids, row), m); err != nil {
			c.w.toast("Could not forward: " + err.Error())
		} else {
			c.w.toast("Forwarded to " + at(names, row))
		}
		d.Close()
	})
	scroll := gtk.NewScrolledWindow()
	scroll.SetChild(list)
	scroll.SetSizeRequest(320, 360)
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.Append(search)
	box.Append(scroll)
	d.SetExtraChild(box)
	d.Present(c.w.win)
}
