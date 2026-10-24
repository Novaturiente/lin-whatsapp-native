package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"lin-whatsapp-native/store"
)

// Picking several messages to copy, forward or delete at once. While any is
// picked, a bar replaces the message box and a click on a message picks it.

func (c *Conversation) buildPickBar() *gtk.ActionBar {
	c.pickBar = gtk.NewActionBar()
	c.pickLabel = gtk.NewLabel("")
	cancel := gtk.NewButtonFromIconName("window-close-symbolic")
	cancel.SetTooltipText("Cancel (Esc)")
	cancel.ConnectClicked(c.clearPicks)
	c.pickBar.PackStart(cancel)
	c.pickBar.PackStart(c.pickLabel)
	c.pickCopy = gtk.NewButtonWithLabel("Copy")
	c.pickCopy.ConnectClicked(c.copyPicks)
	c.pickForward = gtk.NewButtonWithLabel("Forward…")
	c.pickForward.ConnectClicked(func() { c.forward(c.forwardable()...) })
	c.pickDelete = gtk.NewButtonWithLabel("Delete for everyone")
	c.pickDelete.AddCSSClass("destructive-action")
	c.pickDelete.ConnectClicked(func() { c.confirmDelete(c.pickedMessages()...) })
	c.pickBar.PackEnd(c.pickDelete)
	c.pickBar.PackEnd(c.pickForward)
	c.pickBar.PackEnd(c.pickCopy)
	c.pickBar.SetRevealed(false)
	return c.pickBar
}

// pick adds a message to the picked ones, or takes it out again.
func (c *Conversation) pick(id string) {
	slot := c.slots[id]
	m, _ := c.w.c.Store.Message(c.chat, id)
	if slot == nil || m == nil || m.Kind == store.KindSystem || m.Deleted {
		return
	}
	if c.picked[id] {
		delete(c.picked, id)
		slot.RemoveCSSClass("picked")
	} else {
		c.picked[id] = true
		slot.AddCSSClass("picked")
	}
	c.pickChanged()
}

func (c *Conversation) clearPicks() {
	for id := range c.picked {
		if s := c.slots[id]; s != nil {
			s.RemoveCSSClass("picked")
		}
	}
	c.picked = map[string]bool{}
	c.pickChanged()
}

func (c *Conversation) pickChanged() {
	n := len(c.picked)
	c.pickBar.SetRevealed(n > 0)
	c.composer.root.SetVisible(n == 0 && c.chat != "")
	if n == 0 {
		return
	}
	c.pickLabel.SetText(plural(n, "message") + " selected")
	msgs := c.pickedMessages()
	c.pickForward.SetSensitive(len(c.forwardable()) == len(msgs))
	mine := true
	for _, m := range msgs {
		mine = mine && canRevoke(m)
	}
	c.pickDelete.SetSensitive(mine)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// pickedMessages are the picked messages, oldest first.
func (c *Conversation) pickedMessages() []*store.Message {
	var out []*store.Message
	for id := range c.picked {
		if m, _ := c.w.c.Store.Message(c.chat, id); m != nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Time.Equal(out[b].Time) {
			return out[a].Time.Before(out[b].Time)
		}
		return out[a].Seq < out[b].Seq
	})
	return out
}

func (c *Conversation) forwardable() []*store.Message {
	var out []*store.Message
	for _, m := range c.pickedMessages() {
		if canForward(m) {
			out = append(out, m)
		}
	}
	return out
}

// copyPicks copies the picked messages as WhatsApp does:
// "[15:04, 02/01/2006] Name: text", one per line.
func (c *Conversation) copyPicks() {
	var lines []string
	for _, m := range c.pickedMessages() {
		text := m.Text
		if m.Kind != store.KindText || text == "" {
			text = previewOf(m)
		}
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", m.Time.Format("15:04, 02/01/2006"), c.w.c.Name(m.Sender), text))
	}
	c.box.Clipboard().SetText(strings.Join(lines, "\n"))
	c.w.toast(plural(len(lines), "message") + " copied")
	c.clearPicks()
}

func canForward(m *store.Message) bool {
	return !m.Deleted && (len(m.Raw) > 0 ||
		(m.Text != "" && !m.ViewOnce && m.Kind != store.KindOther && m.Kind != store.KindPoll))
}

// canRevoke: WhatsApp lets the author delete for everyone for 48 hours.
func canRevoke(m *store.Message) bool {
	return m.FromMe && !m.Deleted && time.Since(m.Time) < 48*time.Hour
}
