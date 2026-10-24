package ui

import (
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// newChat picks a contact, or checks a typed phone number, and opens that chat.
func (w *Window) newChat() {
	d := adw.NewAlertDialog("New chat", "Pick a contact, or type a number with its country code.")
	d.AddResponse("cancel", "Cancel")
	search := gtk.NewSearchEntry()
	search.SetPlaceholderText("Name or +91 98765 43210")
	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	// What each contact row matches on, by JID; rows are found by widget name.
	match := map[string]string{}
	query := ""
	closed := false
	d.ConnectClosed(func() { closed = true })
	numberRow := gtk.NewListBoxRow()
	numberRow.SetName("number")
	numberLabel := gtk.NewLabel("")
	numberLabel.SetXAlign(0)
	numberLabel.SetMarginTop(8)
	numberLabel.SetMarginBottom(8)
	numberLabel.SetMarginStart(8)
	numberRow.SetChild(numberLabel)
	numberRow.SetVisible(false)
	list.Append(numberRow)

	digits := func() string {
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, search.Text())
	}
	go func() { // the contact list reads the device store; keep it off the UI thread
		all := w.c.Contacts()
		glib.IdleAdd(func() {
			for _, ct := range all {
				match[ct.JID] = strings.ToLower(ct.Name + " " + strings.SplitN(ct.JID, "@", 2)[0])
				row := gtk.NewListBoxRow()
				row.SetName(ct.JID)
				box := gtk.NewBox(gtk.OrientationHorizontal, 8)
				box.SetMarginTop(6)
				box.SetMarginBottom(6)
				box.SetMarginStart(8)
				box.Append(adw.NewAvatar(28, ct.Name, true))
				l := gtk.NewLabel(ct.Name)
				l.SetXAlign(0)
				box.Append(l)
				row.SetChild(box)
				list.Append(row)
			}
			list.InvalidateFilter()
		})
	}()
	list.SetFilterFunc(func(row *gtk.ListBoxRow) bool {
		name := row.Name()
		return name == "number" || strings.Contains(match[name], query)
	})
	search.ConnectSearchChanged(func() {
		query = strings.ToLower(search.Text())
		n := digits()
		numberRow.SetVisible(len(n) >= 8)
		numberLabel.SetText("Message +" + n)
		list.InvalidateFilter()
	})
	open := func(jid string) {
		d.Close()
		w.openChat(jid)
	}
	list.ConnectRowActivated(func(row *gtk.ListBoxRow) {
		if name := row.Name(); name != "number" {
			open(name)
			return
		}
		n := digits() // read the entry here: GTK must not be touched from the goroutine
		numberLabel.SetText("Checking +" + n + "…")
		go func() {
			jid, err := w.c.Lookup(n)
			glib.IdleAdd(func() {
				if closed {
					return
				}
				if err != nil {
					numberLabel.SetText(err.Error())
					return
				}
				open(jid)
			})
		}()
	})
	scroll := gtk.NewScrolledWindow()
	scroll.SetChild(list)
	scroll.SetSizeRequest(340, 380)
	box := gtk.NewBox(gtk.OrientationVertical, 8)
	box.Append(search)
	box.Append(scroll)
	d.SetExtraChild(box)
	d.Present(w.win)
	search.GrabFocus()
}
