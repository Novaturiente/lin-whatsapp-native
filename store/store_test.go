package store

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(id string, sec int64, text string) *Message {
	return &Message{Chat: "1@s.whatsapp.net", ID: id, Sender: "1@s.whatsapp.net",
		Time: time.Unix(sec, 0), Kind: KindText, Text: text}
}

func TestPagingKeepsSameSecondMessages(t *testing.T) {
	s := open(t)
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		if err := s.AddMessage(msg(id, 100+int64(i/3), id), true); err != nil { // a,b,c share a second
			t.Fatal(err)
		}
	}
	var got []string
	var before *Message
	for {
		page, err := s.Messages("1@s.whatsapp.net", before, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for i := len(page) - 1; i >= 0; i-- {
			got = append(got, page[i].ID)
		}
		before = page[0]
	}
	if want := "edcba"; join(got) != want {
		t.Fatalf("paged %q, want %q", join(got), want)
	}
}

func join(v []string) (s string) {
	for _, x := range v {
		s += x
	}
	return
}

func TestDuplicateKeepsStateAndCountsOnce(t *testing.T) {
	s := open(t)
	m := msg("x", 100, "hi")
	m.FromMe, m.Status = true, StatusSent
	if err := s.AddMessage(m, true); err != nil {
		t.Fatal(err)
	}
	s.SetStatus(m.Chat, "x", StatusRead)
	s.SetStatus(m.Chat, "x", StatusDelivered) // late receipt must not go back
	s.SetMedia(m.Chat, "x", "/tmp/f")
	if err := s.AddMessage(msg("x", 100, "hi"), true); err != nil { // history sync overlap
		t.Fatal(err)
	}
	got, _ := s.Message(m.Chat, "x")
	if got.Status != StatusRead || got.Media != "/tmp/f" || !got.FromMe {
		t.Fatalf("replace lost state: %+v", got)
	}
	in := msg("y", 101, "yo")
	s.AddMessage(in, true)
	s.AddMessage(in, true)
	chats, _ := s.Chats()
	if len(chats) != 1 || chats[0].Unread != 1 || chats[0].LastText != "yo" {
		t.Fatalf("chat = %+v", chats)
	}
	s.AddMessage(msg("old", 50, "older"), false) // history: older text must not win
	chats, _ = s.Chats()
	if chats[0].LastText != "yo" {
		t.Fatalf("older message became preview: %q", chats[0].LastText)
	}
}

func TestEditDeleteReactVote(t *testing.T) {
	s := open(t)
	c := "1@s.whatsapp.net"
	s.AddMessage(msg("m", 1, "teh"), false)
	s.Edit(c, "m", "the")
	if m, _ := s.Message(c, "m"); m.Text != "the" || !m.Edited {
		t.Fatalf("edit: %+v", m)
	}
	s.React(c, "m", "a", "👍")
	s.React(c, "m", "a", "❤️")
	s.React(c, "m", "b", "😂")
	s.React(c, "m", "b", "")
	if r, _ := s.Reactions(c, "m"); len(r) != 1 || r[0].Emoji != "❤️" {
		t.Fatalf("reactions: %+v", r)
	}
	s.Vote(c, "m", "a", []string{"x", "y"})
	s.Vote(c, "m", "a", []string{"y"})
	s.Vote(c, "m", "b", []string{"x"})
	s.Vote(c, "m", "b", nil)
	if v, _ := s.Votes(c, "m"); len(v) != 1 || len(v["a"]) != 1 || v["a"][0] != "y" {
		t.Fatalf("votes: %+v", v)
	}
	s.Delete(c, "m")
	if m, _ := s.Message(c, "m"); !m.Deleted || m.Text != "" || Preview(m) != "This message was deleted" {
		t.Fatalf("delete: %+v", m)
	}
	if chats, _ := s.Chats(); chats[0].LastText != "This message was deleted" {
		t.Fatalf("deleted text still in chat list: %q", chats[0].LastText)
	}
}
