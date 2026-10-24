// Package store keeps chats and messages. whatsmeow stores only keys and
// contacts, so everything the window shows comes from here.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Delivery state of a message we sent.
const (
	StatusPending = iota
	StatusSent
	StatusDelivered
	StatusRead
	StatusFailed
)

// Message kinds.
const (
	KindText     = "text"
	KindImage    = "image"
	KindVideo    = "video"
	KindAudio    = "audio" // music or a voice note; Voice tells them apart
	KindDocument = "document"
	KindSticker  = "sticker"
	KindPoll     = "poll"
	KindLocation = "location"
	KindContact  = "contact"
	KindOther    = "other"  // a type this client cannot show yet
	KindSystem   = "system" // a grey centred notice: group changes, missed calls
)

type Message struct {
	Chat        string
	ID          string
	Sender      string
	FromMe      bool
	Time        time.Time
	Kind        string
	Text        string // body, caption, or poll question
	FileName    string
	Mime        string
	Voice       bool
	Animated    bool
	ViewOnce    bool
	Thumb       []byte   // JPEG preview sent inside the message
	Raw         []byte   // marshalled waE2E.Message, needed to download or resend media
	Media       string   // local path once downloaded
	Options     []string // poll options
	MultiVote   bool     // poll allows more than one choice
	QuoteID     string
	QuoteSender string
	QuoteText   string
	Mentions    []string
	Status      int
	Edited      bool
	Deleted     bool
	Seq         int64 // insertion order, the paging cursor's tie-breaker
	// The JIDs exactly as WhatsApp addressed the message (often a hidden
	// LID). Chat and Sender are normalised for display; every message key
	// and receipt about this message must use these instead.
	RawChat, RawSender string
	Forwarded          bool
	// Link preview carried by the message; Thumb holds its picture.
	LinkURL, LinkTitle, LinkDesc string
}

type Chat struct {
	JID       string
	Name      string
	LastTime  time.Time
	LastText  string
	Unread    int
	Ephemeral uint32 // disappearing-messages timer in seconds, 0 when off
}

type Reaction struct{ Sender, Emoji string }

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '',
	last_ts INTEGER NOT NULL DEFAULT 0, last_text TEXT NOT NULL DEFAULT '',
	unread INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS messages (
	chat TEXT NOT NULL, id TEXT NOT NULL, sender TEXT NOT NULL, from_me INTEGER NOT NULL,
	ts INTEGER NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL, file_name TEXT NOT NULL,
	mime TEXT NOT NULL, voice INTEGER NOT NULL, animated INTEGER NOT NULL,
	view_once INTEGER NOT NULL, thumb BLOB, raw BLOB, media TEXT NOT NULL DEFAULT '',
	options TEXT NOT NULL, multi_vote INTEGER NOT NULL,
	quote_id TEXT NOT NULL, quote_sender TEXT NOT NULL, quote_text TEXT NOT NULL,
	mentions TEXT NOT NULL, status INTEGER NOT NULL, edited INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	raw_chat TEXT NOT NULL DEFAULT '', raw_sender TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (chat, id));
CREATE INDEX IF NOT EXISTS messages_by_time ON messages (chat, ts);
CREATE TABLE IF NOT EXISTS reactions (
	chat TEXT NOT NULL, msg_id TEXT NOT NULL, sender TEXT NOT NULL, emoji TEXT NOT NULL,
	PRIMARY KEY (chat, msg_id, sender));
CREATE TABLE IF NOT EXISTS poll_votes (
	chat TEXT NOT NULL, poll_id TEXT NOT NULL, voter TEXT NOT NULL, options TEXT NOT NULL,
	PRIMARY KEY (chat, poll_id, voter));
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; SQLite serialises anyway
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db}, migrate(db)
}

// migrations change the schema above, which stays frozen: chat.db already
// holds people's messages, so columns are only ever added. Entry i brings
// the database to user_version i+1.
var migrations = []string{
	`ALTER TABLE chats ADD COLUMN ephemeral INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE messages ADD COLUMN forwarded INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE messages ADD COLUMN link_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE messages ADD COLUMN link_title TEXT NOT NULL DEFAULT '';
	ALTER TABLE messages ADD COLUMN link_desc TEXT NOT NULL DEFAULT '';`,
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func js(v []string) string {
	if v == nil {
		v = []string{}
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// Preview is the one line the chat list shows for a message.
func Preview(m *Message) string {
	if m.Deleted {
		return "This message was deleted"
	}
	label := map[string]string{
		KindImage: "📷 Photo", KindVideo: "🎥 Video", KindAudio: "🎵 Audio",
		KindDocument: "📄 ", KindSticker: "Sticker", KindPoll: "📊 ",
		KindLocation: "📍 Location", KindContact: "👤 ", KindOther: "Unsupported message",
	}[m.Kind]
	switch {
	case m.Kind == KindAudio && m.Voice:
		return "🎤 Voice message"
	case m.Kind == KindDocument:
		return label + m.FileName
	case m.Kind == KindPoll || m.Kind == KindContact:
		return label + m.Text
	case m.Text != "":
		if label != "" {
			return label + ": " + m.Text
		}
		return m.Text
	}
	return label
}

// AddMessage inserts a message, or replaces one with the same ID (a resend
// or history sync overlap), and moves its chat to the top. A replace keeps
// the downloaded file, the delivery state and edit/delete marks already known.
func (s *Store) AddMessage(m *Message, countUnread bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO messages (chat, id, sender, from_me, ts, kind, text,
		file_name, mime, voice, animated, view_once, thumb, raw, media, options, multi_vote,
		quote_id, quote_sender, quote_text, mentions, status, edited, deleted, raw_chat, raw_sender,
		forwarded, link_url, link_title, link_desc)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (chat, id) DO NOTHING`,
		m.Chat, m.ID, m.Sender, b2i(m.FromMe), m.Time.UnixMilli(), m.Kind, m.Text,
		m.FileName, m.Mime, b2i(m.Voice), b2i(m.Animated), b2i(m.ViewOnce), m.Thumb, m.Raw,
		m.Media, js(m.Options), b2i(m.MultiVote), m.QuoteID, m.QuoteSender, m.QuoteText,
		js(m.Mentions), m.Status, b2i(m.Edited), b2i(m.Deleted), m.RawChat, m.RawSender,
		b2i(m.Forwarded), m.LinkURL, m.LinkTitle, m.LinkDesc)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit() // already known
	}
	unread := 0
	if countUnread && !m.FromMe {
		unread = 1
	}
	if _, err = tx.Exec(`INSERT INTO chats (jid, last_ts, last_text, unread) VALUES (?,?,?,?)
		ON CONFLICT (jid) DO UPDATE SET unread = unread + excluded.unread,
		last_ts = max(last_ts, excluded.last_ts),
		last_text = CASE WHEN excluded.last_ts >= last_ts THEN excluded.last_text ELSE last_text END`,
		m.Chat, m.Time.UnixMilli(), Preview(m), unread); err != nil {
		return err
	}
	return tx.Commit()
}

const cols = `rowid, chat, id, sender, from_me, ts, kind, text, file_name, mime, voice, animated,
	view_once, thumb, raw, media, options, multi_vote, quote_id, quote_sender, quote_text,
	mentions, status, edited, deleted, raw_chat, raw_sender, forwarded, link_url, link_title, link_desc`

func scan(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var ts int64
	var fromMe, voice, animated, viewOnce, multi, edited, deleted, forwarded int
	var options, mentions string
	err := row.Scan(&m.Seq, &m.Chat, &m.ID, &m.Sender, &fromMe, &ts, &m.Kind, &m.Text, &m.FileName,
		&m.Mime, &voice, &animated, &viewOnce, &m.Thumb, &m.Raw, &m.Media, &options, &multi,
		&m.QuoteID, &m.QuoteSender, &m.QuoteText, &mentions, &m.Status, &edited, &deleted,
		&m.RawChat, &m.RawSender, &forwarded, &m.LinkURL, &m.LinkTitle, &m.LinkDesc)
	if err != nil {
		return nil, err
	}
	m.Forwarded = forwarded == 1
	m.Time = time.UnixMilli(ts)
	m.FromMe, m.Voice, m.Animated, m.ViewOnce = fromMe == 1, voice == 1, animated == 1, viewOnce == 1
	m.MultiVote, m.Edited, m.Deleted = multi == 1, edited == 1, deleted == 1
	_ = json.Unmarshal([]byte(options), &m.Options)
	_ = json.Unmarshal([]byte(mentions), &m.Mentions)
	return &m, nil
}

// Message returns one message, or nil if it is not stored.
func (s *Store) Message(chat, id string) (*Message, error) {
	m, err := scan(s.db.QueryRow(`SELECT `+cols+` FROM messages WHERE chat = ? AND id = ?`, chat, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// Messages returns up to limit messages older than before, oldest first.
// A nil before means the newest. Many messages share a second, so the cursor
// is (time, insertion order), never time alone.
func (s *Store) Messages(chat string, before *Message, limit int) ([]*Message, error) {
	until, seq := int64(1<<62), int64(0)
	if before != nil {
		until, seq = before.Time.UnixMilli(), before.Seq
	}
	rows, err := s.db.Query(`SELECT `+cols+` FROM messages WHERE chat = ?
		AND (ts < ? OR (ts = ? AND rowid < ?))
		ORDER BY ts DESC, rowid DESC LIMIT ?`, chat, until, until, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append([]*Message{m}, out...)
	}
	return out, rows.Err()
}

// Media returns a chat's newest photos and videos, oldest first, for
// stepping through them in the viewer.
// shortcut: only the newest 500, page like Messages if chats hold more.
func (s *Store) Media(chat string) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT `+cols+` FROM messages WHERE chat = ? AND kind IN (?, ?)
		AND deleted = 0 AND view_once = 0 ORDER BY ts DESC, rowid DESC LIMIT 500`, chat, KindImage, KindVideo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append([]*Message{m}, out...)
	}
	return out, rows.Err()
}

// Stickers returns distinct stickers seen in any chat, newest first, so they
// can be sent again without uploading.
func (s *Store) Stickers(limit int) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT `+cols+` FROM messages WHERE kind = ? AND deleted = 0
		AND media != '' GROUP BY media ORDER BY max(ts) DESC LIMIT ?`, KindSticker, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) Chats() ([]Chat, error) {
	rows, err := s.db.Query(`SELECT jid, name, last_ts, last_text, unread, ephemeral FROM chats ORDER BY last_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		var ts int64
		if err := rows.Scan(&c.JID, &c.Name, &ts, &c.LastText, &c.Unread, &c.Ephemeral); err != nil {
			return nil, err
		}
		c.LastTime = time.UnixMilli(ts)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetChatName records a name; an empty one never overwrites a known one.
func (s *Store) SetChatName(jid, name string) error {
	if name == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO chats (jid, name) VALUES (?,?)
		ON CONFLICT (jid) DO UPDATE SET name = excluded.name`, jid, name)
	return err
}

// Chat returns one chat; a chat never stored comes back empty with its JID.
func (s *Store) Chat(jid string) (Chat, error) {
	c := Chat{JID: jid}
	var ts int64
	err := s.db.QueryRow(`SELECT name, last_ts, last_text, unread, ephemeral FROM chats WHERE jid = ?`, jid).
		Scan(&c.Name, &ts, &c.LastText, &c.Unread, &c.Ephemeral)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	c.LastTime = time.UnixMilli(ts)
	return c, err
}

// SetEphemeral records a chat's disappearing-messages timer in seconds. A
// zero never creates a chat row: empty conversations must not show up blank.
func (s *Store) SetEphemeral(jid string, seconds uint32) error {
	if seconds == 0 {
		_, err := s.db.Exec(`UPDATE chats SET ephemeral = 0 WHERE jid = ?`, jid)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO chats (jid, ephemeral) VALUES (?,?)
		ON CONFLICT (jid) DO UPDATE SET ephemeral = excluded.ephemeral`, jid, seconds)
	return err
}

// OldestReal is the oldest message the phone also has, the anchor for asking
// it for older ones: notice lines are this client's own.
func (s *Store) OldestReal(chat string) (*Message, error) {
	m, err := scan(s.db.QueryRow(`SELECT `+cols+` FROM messages WHERE chat = ? AND kind != ?
		ORDER BY ts, rowid LIMIT 1`, chat, KindSystem))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (s *Store) SetUnread(jid string, n int) error {
	_, err := s.db.Exec(`UPDATE chats SET unread = ? WHERE jid = ?`, n, jid)
	return err
}

// Unread returns the newest limit incoming messages, for read receipts.
func (s *Store) Unread(chat string, limit int) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT `+cols+` FROM messages WHERE chat = ? AND from_me = 0
		AND kind != ? ORDER BY ts DESC LIMIT ?`, chat, KindSystem, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetStatus raises the delivery state; receipts arrive out of order, so it
// never goes backwards.
func (s *Store) SetStatus(chat, id string, status int) error {
	_, err := s.db.Exec(`UPDATE messages SET status = ? WHERE chat = ? AND id = ? AND status < ?`,
		status, chat, id, status)
	return err
}

// ResetStatus puts a failed message back to pending for a retry.
func (s *Store) ResetStatus(chat, id string) error {
	_, err := s.db.Exec(`UPDATE messages SET status = ? WHERE chat = ? AND id = ?`, StatusPending, chat, id)
	return err
}

func (s *Store) SetMedia(chat, id, path string) error {
	_, err := s.db.Exec(`UPDATE messages SET media = ? WHERE chat = ? AND id = ?`, path, chat, id)
	return err
}

func (s *Store) Edit(chat, id, text string) error {
	if _, err := s.db.Exec(`UPDATE messages SET text = ?, edited = 1 WHERE chat = ? AND id = ?`, text, chat, id); err != nil {
		return err
	}
	return s.refreshPreview(chat, id)
}

func (s *Store) Delete(chat, id string) error {
	if _, err := s.db.Exec(`UPDATE messages SET deleted = 1, text = '', raw = NULL, thumb = NULL
		WHERE chat = ? AND id = ?`, chat, id); err != nil {
		return err
	}
	return s.refreshPreview(chat, id)
}

// refreshPreview rewrites the chat list line when the changed message is the
// newest one, so a deleted message's text does not linger there.
func (s *Store) refreshPreview(chat, id string) error {
	newest, err := s.Messages(chat, nil, 1)
	if err != nil || len(newest) == 0 || newest[0].ID != id {
		return err
	}
	_, err = s.db.Exec(`UPDATE chats SET last_text = ? WHERE jid = ?`, Preview(newest[0]), chat)
	return err
}

// React sets or, with an empty emoji, removes one sender's reaction.
func (s *Store) React(chat, msgID, sender, emoji string) error {
	if emoji == "" {
		_, err := s.db.Exec(`DELETE FROM reactions WHERE chat = ? AND msg_id = ? AND sender = ?`, chat, msgID, sender)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO reactions VALUES (?,?,?,?)
		ON CONFLICT (chat, msg_id, sender) DO UPDATE SET emoji = excluded.emoji`, chat, msgID, sender, emoji)
	return err
}

func (s *Store) Reactions(chat, msgID string) ([]Reaction, error) {
	rows, err := s.db.Query(`SELECT sender, emoji FROM reactions WHERE chat = ? AND msg_id = ?`, chat, msgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reaction
	for rows.Next() {
		var r Reaction
		if err := rows.Scan(&r.Sender, &r.Emoji); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Vote replaces a voter's choices in a poll; an empty list withdraws the vote.
func (s *Store) Vote(chat, pollID, voter string, options []string) error {
	if len(options) == 0 {
		_, err := s.db.Exec(`DELETE FROM poll_votes WHERE chat = ? AND poll_id = ? AND voter = ?`, chat, pollID, voter)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO poll_votes VALUES (?,?,?,?)
		ON CONFLICT (chat, poll_id, voter) DO UPDATE SET options = excluded.options`,
		chat, pollID, voter, js(options))
	return err
}

// Votes maps each voter to the options they chose.
func (s *Store) Votes(chat, pollID string) (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT voter, options FROM poll_votes WHERE chat = ? AND poll_id = ?`, chat, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var voter, options string
		if err := rows.Scan(&voter, &options); err != nil {
			return nil, err
		}
		var list []string
		_ = json.Unmarshal([]byte(options), &list)
		out[voter] = list
	}
	return out, rows.Err()
}
