package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"

	"lin-whatsapp-native/store"
	"lin-whatsapp-native/wa"
)

// Dialogs that show information: starred messages, chat and group info,
// and who received and read a message.

// infoRow is one line of a dialog list: a title and a dim subtitle.
func infoRow(title, subtitle string) *gtk.ListBoxRow {
	t := gtk.NewLabel(title)
	t.SetXAlign(0)
	t.SetEllipsize(pango.EllipsizeEnd)
	box := gtk.NewBox(gtk.OrientationVertical, 2)
	box.SetMarginTop(6)
	box.SetMarginBottom(6)
	box.SetMarginStart(8)
	box.SetMarginEnd(8)
	box.Append(t)
	if subtitle != "" {
		s := gtk.NewLabel(subtitle)
		s.SetXAlign(0)
		s.SetWrap(true)
		s.SetWrapMode(pango.WrapWordChar)
		s.SetLines(3)
		s.SetEllipsize(pango.EllipsizeEnd)
		s.AddCSSClass("dim-label")
		box.Append(s)
	}
	row := gtk.NewListBoxRow()
	row.SetChild(box)
	return row
}

func scrolled(child gtk.Widgetter, height int) *gtk.ScrolledWindow {
	s := gtk.NewScrolledWindow()
	s.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	s.SetChild(child)
	s.SetPropagateNaturalHeight(true)
	s.SetMaxContentHeight(height)
	s.SetSizeRequest(340, -1)
	return s
}

func longTime(t time.Time) string { return t.Format("2 Jan 2006, 15:04") }

// showStarred lists starred messages from every chat; picking one opens it.
func (w *Window) showStarred() {
	msgs, _ := w.c.Store.Starred(300)
	d := adw.NewAlertDialog("Starred messages", "")
	d.AddResponse("close", "Close")
	if len(msgs) == 0 {
		d.SetBody("Star a message from its menu to find it here.")
		d.Present(w.win)
		return
	}
	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	for _, m := range msgs {
		who := w.c.Name(m.Sender)
		if where := w.c.Name(m.Chat); isGroup(m.Chat) {
			who += " in " + where
		}
		list.Append(infoRow(who+" · "+shortTime(m.Time), firstLine(previewOf(m))))
	}
	list.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		m := msgs[row.Index()]
		d.Close()
		w.openChat(m.Chat)
		if w.conv.jumpTo(m.ID) {
			w.conv.moveCursor(w.conv.slots[m.ID])
		}
	})
	d.SetExtraChild(scrolled(list, 420))
	d.Present(w.win)
}

// chatInfo shows a chat's details; for a group, its description and members.
func (w *Window) chatInfo(chat string) {
	d := adw.NewAlertDialog(w.c.Name(chat), "")
	d.AddResponse("close", "Close")
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	d.SetExtraChild(box)

	if user, server, _ := strings.Cut(chat, "@"); server == "s.whatsapp.net" { // not a hidden LID
		d.SetBody("+" + user)
	}
	if items, _ := w.c.Store.Media(chat); len(items) > 0 {
		b := gtk.NewButtonWithLabel("Photos and videos")
		b.ConnectClicked(func() {
			d.Close()
			w.openChat(chat)
			w.conv.view(items[len(items)-1]) // Media is oldest first
		})
		box.Append(b)
	}
	if isGroup(chat) {
		wait := gtk.NewSpinner()
		wait.Start()
		box.Append(wait)
		go func() {
			g, err := w.c.Group(chat)
			glib.IdleAdd(func() {
				box.Remove(wait)
				if err != nil {
					d.SetBody("Could not load the group: " + err.Error())
					return
				}
				w.groupDetails(d, box, g)
			})
		}()
	}
	d.Present(w.win)
}

func (w *Window) groupDetails(d *adw.AlertDialog, box *gtk.Box, g *wa.Group) {
	body := g.Topic
	if !g.Created.IsZero() {
		if body != "" {
			body += "\n\n"
		}
		body += "Created " + longTime(g.Created)
	}
	d.SetBody(body)
	head := gtk.NewLabel(fmt.Sprintf("%d members", len(g.Members)))
	head.SetXAlign(0)
	head.AddCSSClass("heading")
	box.Append(head)
	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	list.SetSelectionMode(gtk.SelectionNone)
	for _, m := range g.Members {
		role := ""
		if m.Admin {
			role = "Admin"
		}
		list.Append(infoRow(w.c.Name(m.JID), role))
	}
	box.Append(scrolled(list, 360))
}

// messageInfo shows who got and read one of our messages.
func (w *Window) messageInfo(m *store.Message) {
	d := adw.NewAlertDialog("Message info", "Only receipts that arrived while this app was running are shown.")
	d.AddResponse("close", "Close")
	list, _ := w.c.Store.Receipts(m.Chat, m.ID)
	if len(list) == 0 {
		d.SetBody("No receipts yet. " + d.Body())
		d.Present(w.win)
		return
	}
	lb := gtk.NewListBox()
	lb.AddCSSClass("boxed-list")
	lb.SetSelectionMode(gtk.SelectionNone)
	for _, r := range list {
		var parts []string
		if !r.Read.IsZero() {
			parts = append(parts, "Read "+longTime(r.Read))
		}
		if !r.Delivered.IsZero() {
			parts = append(parts, "Delivered "+longTime(r.Delivered))
		}
		lb.Append(infoRow(w.c.Name(r.Member), strings.Join(parts, " · ")))
	}
	d.SetExtraChild(scrolled(lb, 360))
	d.Present(w.win)
}
