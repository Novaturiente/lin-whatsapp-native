package ui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"

	"lin-whatsapp-native/store"
	"lin-whatsapp-native/wa"
)

// Run with MENTION_GTK_TEST=1 go test ./ui -run TestMentionKeyboard -count=1
// on a working display; no WhatsApp account or network connection is used.
func TestMentionKeyboard(t *testing.T) {
	if os.Getenv("MENTION_GTK_TEST") != "1" {
		t.Skip("set MENTION_GTK_TEST=1 with a working display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	client, err := wa.Open(context.Background(), dir, st)
	if err != nil {
		t.Fatal(err)
	}
	container := client.WA.Store.Container.(*sqlstore.Container)
	defer container.Close()
	client.WA.Store.Contacts = sqlstore.NewSQLStore(container, types.NewJID("999", types.DefaultUserServer))
	k := &Composer{
		c:    &Conversation{w: &Window{c: client}},
		view: gtk.NewTextView(), mentionList: gtk.NewListBox(), mentionPop: gtk.NewPopover(),
		mentions: map[string]string{}, members: []string{"111@s.whatsapp.net", "222@s.whatsapp.net"},
	}
	win := gtk.NewWindow()
	win.SetChild(k.view)
	k.mentionPop.SetParent(k.view)
	k.mentionPop.SetAutohide(false)
	k.mentionPop.SetChild(k.mentionList)
	defer func() { k.mentionPop.Unparent(); win.Destroy() }()
	win.Present()
	k.view.GrabFocus()
	ctx := glib.MainContextDefault()
	for ctx.Pending() {
		ctx.Iteration(false)
	}
	open := func() {
		k.view.Buffer().SetText("@")
		k.updateMentions()
		if !k.mentionPop.Visible() || k.mentionList.SelectedRow().Index() != 0 {
			t.Fatal("popup must select first match")
		}
	}
	for _, accept := range []uint{gdk.KEY_Return, gdk.KEY_Tab, gdk.KEY_KP_Enter} {
		open()
		for _, step := range []struct {
			key  uint
			want int
		}{{gdk.KEY_Up, 0}, {gdk.KEY_Down, 1}, {gdk.KEY_Down, 1}, {gdk.KEY_Up, 0}, {gdk.KEY_Down, 1}} {
			if !k.key(step.key, 0, 0) || k.mentionList.SelectedRow().Index() != step.want {
				t.Fatalf("key %d: selection must be %d", step.key, step.want)
			}
		}
		if !k.view.IsFocus() {
			t.Fatal("arrow selection moved focus away from composer")
		}
		if !k.key(accept, 0, 0) || k.text() != "@+222 " || k.mentions["@+222"] != "222@s.whatsapp.net" || k.mentionPop.Visible() {
			t.Fatalf("accept key %d: text=%q mentions=%v", accept, k.text(), k.mentions)
		}
	}
	open()
	if !k.key(gdk.KEY_Escape, 0, 0) || k.mentionPop.Visible() {
		t.Fatal("Escape must close suggestions")
	}
	if k.key(gdk.KEY_Down, 0, 0) || k.key(gdk.KEY_a, 0, 0) || k.key(gdk.KEY_Return, 0, gdk.ShiftMask) {
		t.Fatal("closed suggestions must preserve normal text editing")
	}
}
