package store

import (
	"database/sql"
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

// An existing chat.db from before the migrations keeps its messages.
func TestMigrateKeepsOldData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO messages (chat, id, sender, from_me, ts, kind, text, file_name,
		mime, voice, animated, view_once, options, multi_vote, quote_id, quote_sender, quote_text,
		mentions, status) VALUES ('c','old','s',0,1,'text','kept','','',0,0,0,'[]',0,'','','','[]',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO messages (chat, id, sender, from_me, ts, kind, text, file_name,
		mime, voice, animated, view_once, options, multi_vote, quote_id, quote_sender, quote_text,
		mentions, status) VALUES ('c','call-1','c',0,2,'system','📞 Incoming voice call from X','','',0,0,0,'[]',0,'','','','[]',1);
		INSERT INTO chats (jid, last_ts, last_text) VALUES ('c', 2, '📞 Incoming voice call from X')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, err := s.Message("c", "old"); err != nil || m == nil || m.Text != "kept" {
		t.Fatalf("old message after migration: %+v %v", m, err)
	}
	if m, _ := s.Message("c", "call-1"); m != nil {
		t.Fatalf("call notice survived migration 2")
	}
	if ch, _ := s.Chat("c"); ch.LastText != "kept" {
		t.Fatalf("chat preview still %q", ch.LastText)
	}
	if err := s.SetEphemeral("c", 86400); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Chat("c"); c.Ephemeral != 86400 {
		t.Fatalf("ephemeral = %d", c.Ephemeral)
	}
	s.Close()
	if s, err = Open(path); err != nil { // reopening must not re-run migrations
		t.Fatal(err)
	}
}

func TestSystemLinesStayOutOfReceipts(t *testing.T) {
	s := open(t)
	c := "1@s.whatsapp.net"
	s.AddMessage(&Message{Chat: c, ID: "sys-1", Sender: c, Time: time.Unix(1, 0), Kind: KindSystem, Text: "X left"}, false)
	s.AddMessage(msg("real", 2, "hi"), true)
	s.AddMessage(&Message{Chat: c, ID: "call-1", Sender: c, Time: time.Unix(3, 0), Kind: KindSystem, Text: "call"}, false)
	if got, _ := s.Unread(c, 1); len(got) != 1 || got[0].ID != "real" {
		t.Fatalf("unread: %+v", got)
	}
	if m, _ := s.OldestReal(c); m == nil || m.ID != "real" {
		t.Fatalf("oldest real: %+v", m)
	}
	s.SetEphemeral("empty@s.whatsapp.net", 0)
	if chats, _ := s.Chats(); len(chats) != 1 {
		t.Fatalf("a zero timer created a chat: %+v", chats)
	}
}

func TestSearch(t *testing.T) {
	s := open(t)
	c := "1@s.whatsapp.net"
	s.AddMessage(msg("a", 1, "Meeting at 5"), false)
	s.AddMessage(msg("b", 2, "no meeting today"), false)
	s.AddMessage(msg("c", 3, "100% sure"), false)
	s.AddMessage(msg("d", 4, "1000 sure"), false)
	if got, _ := s.Search(c, "MEETING", 10); len(got) != 2 || got[0].ID != "b" {
		t.Fatalf("case-insensitive, newest first: %+v", got)
	}
	if got, _ := s.Search(c, "100%", 10); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("%% must match literally: %+v", got)
	}
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
