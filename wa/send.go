package wa

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"lin-whatsapp-native/media"
	"lin-whatsapp-native/store"
)

// Every send shows the message at once as pending, then sends in the
// background and moves it to sent or failed. Errors come back as EvUpdate.

func jid(s string) types.JID {
	j, _ := types.ParseJID(s)
	return j
}

// quote builds the reply block for a message being answered, or nil.
func (c *Client) quote(reply *store.Message, mentions []string) *waE2E.ContextInfo {
	if reply == nil && len(mentions) == 0 {
		return nil
	}
	ci := &waE2E.ContextInfo{MentionedJID: mentions}
	if reply != nil {
		quoted := &waE2E.Message{Conversation: proto.String(reply.Text)}
		if len(reply.Raw) > 0 {
			var full waE2E.Message
			if proto.Unmarshal(reply.Raw, &full) == nil {
				quoted = &full
			}
		}
		ci.StanzaID, ci.Participant, ci.QuotedMessage = proto.String(reply.ID), proto.String(c.sender(reply).String()), quoted
	}
	return ci
}

// send stores msg as pending in chat, then delivers it. localMedia, when
// set, is the file the message was made from, so it shows without a download.
func (c *Client) send(chat string, msg *waE2E.Message, localMedia string) {
	if ch, _ := c.Store.Chat(chat); ch.Ephemeral > 0 {
		msg = withExpiration(msg, ch.Ephemeral)
	}
	id := c.WA.GenerateMessageID()
	e := &events.Message{Message: msg, Info: types.MessageInfo{ID: id, Timestamp: time.Now(),
		MessageSource: types.MessageSource{Chat: jid(chat), Sender: c.Me(), IsFromMe: true}}}
	if u := Convert(e, chat, c.Me().String()); u.Msg != nil {
		u.Msg.Status, u.Msg.Media = store.StatusPending, localMedia
		u.Msg.RawChat, u.Msg.RawSender = chat, c.Me().String()
		_ = c.Store.AddMessage(u.Msg, false)
		c.OnEvent(Event{Kind: EvMessage, Chat: chat, ID: id})
	}
	c.transmit(chat, id, msg)
}

// transmit sends with a fixed ID. A failed message stays in outbox so Retry
// sends the very same message again; the shared ID means the recipient never
// sees it twice.
func (c *Client) transmit(chat, id string, msg *waE2E.Message) {
	c.mu.Lock()
	c.outbox[id] = msg
	c.mu.Unlock()
	go func() {
		_, err := c.WA.SendMessage(context.Background(), jid(chat), msg, whatsmeow.SendRequestExtra{ID: id})
		status := store.StatusSent
		if err != nil {
			fmt.Fprintln(os.Stderr, "send:", err)
			status = store.StatusFailed
		} else {
			c.mu.Lock()
			delete(c.outbox, id)
			c.mu.Unlock()
		}
		_ = c.Store.SetStatus(chat, id, status)
		c.OnEvent(Event{Kind: EvUpdate, Chat: chat, ID: id})
	}()
}

// Retry sends a failed message again. After a restart the outbox is empty,
// so it is rebuilt from what the store kept: the text, or the uploaded media.
// shortcut: a rebuilt text loses its quote and mentions; persist the outbox
// if retries after a restart turn out common.
func (c *Client) Retry(m *store.Message) error {
	c.mu.Lock()
	msg := c.outbox[m.ID]
	c.mu.Unlock()
	if msg == nil {
		msg = &waE2E.Message{}
		if len(m.Raw) > 0 {
			if err := proto.Unmarshal(m.Raw, msg); err != nil {
				return err
			}
		} else if m.Kind == store.KindText {
			msg.Conversation = proto.String(m.Text)
		} else {
			return fmt.Errorf("this message can no longer be resent")
		}
		if ch, _ := c.Store.Chat(m.Chat); ch.Ephemeral > 0 {
			msg = withExpiration(msg, ch.Ephemeral)
		}
	}
	if err := c.Store.ResetStatus(m.Chat, m.ID); err != nil {
		return err
	}
	c.OnEvent(Event{Kind: EvUpdate, Chat: m.Chat, ID: m.ID})
	c.transmit(m.Chat, m.ID, msg)
	return nil
}

// withExpiration marks a message for a chat with disappearing messages on.
// Without it the message stays forever on everyone else's phone.
func withExpiration(msg *waE2E.Message, seconds uint32) *waE2E.Message {
	if msg.Conversation != nil { // plain text has no ContextInfo to carry the timer
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: msg.Conversation}}
	}
	var slot **waE2E.ContextInfo
	switch {
	case msg.ExtendedTextMessage != nil:
		slot = &msg.ExtendedTextMessage.ContextInfo
	case msg.ImageMessage != nil:
		slot = &msg.ImageMessage.ContextInfo
	case msg.VideoMessage != nil:
		slot = &msg.VideoMessage.ContextInfo
	case msg.AudioMessage != nil:
		slot = &msg.AudioMessage.ContextInfo
	case msg.DocumentMessage != nil:
		slot = &msg.DocumentMessage.ContextInfo
	case msg.StickerMessage != nil:
		slot = &msg.StickerMessage.ContextInfo
	case msg.PollCreationMessageV3 != nil:
		slot = &msg.PollCreationMessageV3.ContextInfo
	case msg.PollCreationMessage != nil:
		slot = &msg.PollCreationMessage.ContextInfo
	default:
		return msg
	}
	if *slot == nil {
		*slot = &waE2E.ContextInfo{}
	}
	(*slot).Expiration = proto.Uint32(seconds)
	return msg
}

func (c *Client) SendText(chat, text string, reply *store.Message, mentions []string) {
	if ci := c.quote(reply, mentions); ci != nil {
		c.send(chat, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text), ContextInfo: ci}}, "")
		return
	}
	c.send(chat, &waE2E.Message{Conversation: proto.String(text)}, "")
}

// SendFile sends a file the way WhatsApp would: photos and MP4 video inline,
// audio as audio, everything else as a document. Animated GIFs go as
// documents, since WhatsApp only animates the MP4 it converts them into.
// asDocument forces a document, which keeps photos at full quality.
// It reads, previews and uploads in the background; failures arrive as EvError.
func (c *Client) SendFile(chat, path, caption string, reply *store.Message, asDocument bool) {
	go func() {
		if err := c.sendFile(chat, path, caption, reply, asDocument); err != nil {
			c.OnEvent(Event{Kind: EvError, Text: "Could not send " + filepath.Base(path) + ": " + err.Error()})
		}
	}()
}

func (c *Client) sendFile(chat, path, caption string, reply *store.Message, asDocument bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	mimeType := media.Type(path)
	kind := whatsmeow.MediaDocument
	switch {
	case asDocument:
	case strings.HasPrefix(mimeType, "image/") && mimeType != "image/gif":
		kind = whatsmeow.MediaImage
	case mimeType == "video/mp4":
		kind = whatsmeow.MediaVideo
	case strings.HasPrefix(mimeType, "audio/"):
		kind = whatsmeow.MediaAudio
	}
	var img = struct {
		thumb []byte
		w, h  int
	}{}
	if kind == whatsmeow.MediaImage {
		decoded, err := media.Decode(path)
		if err != nil {
			kind = whatsmeow.MediaDocument // not a picture WhatsApp could show
		} else {
			img.thumb, img.w, img.h = media.Thumbnail(decoded, 100)
		}
	}
	up, err := c.WA.Upload(context.Background(), data, kind)
	if err != nil {
		return err
	}
	local := c.keep(path, data)
	{
		ci := c.quote(reply, nil)
		m := &waE2E.Message{}
		switch kind {
		case whatsmeow.MediaImage:
			m.ImageMessage = &waE2E.ImageMessage{Caption: optional(caption), Mimetype: proto.String(mimeType),
				JPEGThumbnail: img.thumb, Width: proto.Uint32(uint32(img.w)), Height: proto.Uint32(uint32(img.h)),
				URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
				FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: ci}
		case whatsmeow.MediaVideo:
			m.VideoMessage = &waE2E.VideoMessage{Caption: optional(caption), Mimetype: proto.String(mimeType),
				JPEGThumbnail: media.VideoThumbnail(path), Seconds: proto.Uint32(media.Seconds(path)),
				URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
				FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: ci}
		case whatsmeow.MediaAudio:
			m.AudioMessage = &waE2E.AudioMessage{Mimetype: proto.String(mimeType), Seconds: proto.Uint32(media.Seconds(path)),
				URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
				FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: ci}
		default:
			name := filepath.Base(path)
			m.DocumentMessage = &waE2E.DocumentMessage{Caption: optional(caption), Mimetype: proto.String(mimeType),
				FileName: &name, Title: &name,
				URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
				FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: ci}
		}
		c.send(chat, m, local)
	}
	return nil
}

// SendVoice sends a recorded Ogg/Opus file as a voice note, then removes
// the recording.
func (c *Client) SendVoice(chat, path string, reply *store.Message) {
	go func() {
		defer os.Remove(path)
		data, err := os.ReadFile(path)
		if err == nil {
			var up whatsmeow.UploadResponse
			if up, err = c.WA.Upload(context.Background(), data, whatsmeow.MediaAudio); err == nil {
				c.sendVoice(chat, path, c.keep(path, data), up, reply)
			}
		}
		if err != nil {
			c.OnEvent(Event{Kind: EvError, Text: "Could not send voice message: " + err.Error()})
		}
	}()
}

func (c *Client) sendVoice(chat, path, local string, up whatsmeow.UploadResponse, reply *store.Message) {
	{
		c.send(chat, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true),
			Mimetype: proto.String("audio/ogg; codecs=opus"), Seconds: proto.Uint32(media.Seconds(path)),
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, ContextInfo: c.quote(reply, nil)}}, local)
	}
}

// SendSticker sends a sticker seen before. Its upload is still valid, so it
// goes out as is, the way WhatsApp's own sticker tray does it.
func (c *Client) SendSticker(chat string, sticker *store.Message, reply *store.Message) error {
	var m waE2E.Message
	if err := proto.Unmarshal(sticker.Raw, &m); err != nil || m.StickerMessage == nil {
		return fmt.Errorf("not a sticker")
	}
	m.StickerMessage.ContextInfo = c.quote(reply, nil)
	c.send(chat, &waE2E.Message{StickerMessage: m.StickerMessage}, sticker.Media)
	return nil
}

// Forward sends a stored message to another chat without uploading again.
func (c *Client) Forward(to string, m *store.Message) error {
	if len(m.Raw) == 0 {
		c.SendText(to, m.Text, nil, nil)
		return nil
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(m.Raw, &msg); err != nil {
		return err
	}
	ci := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)}
	switch {
	case msg.ImageMessage != nil:
		msg.ImageMessage.ContextInfo = ci
	case msg.VideoMessage != nil:
		msg.VideoMessage.ContextInfo = ci
	case msg.PtvMessage != nil:
		msg.PtvMessage.ContextInfo = ci
	case msg.AudioMessage != nil:
		msg.AudioMessage.ContextInfo = ci
	case msg.DocumentMessage != nil:
		msg.DocumentMessage.ContextInfo = ci
	case msg.StickerMessage != nil:
		msg.StickerMessage.ContextInfo = ci
	}
	c.send(to, &msg, m.Media)
	return nil
}

func (c *Client) SendPoll(chat, question string, options []string, multi bool) {
	count := 1
	if multi {
		count = 0 // 0 lets each voter choose any number
	}
	c.send(chat, c.WA.BuildPollCreation(question, options, count), "")
}

// keep copies a file being sent into the media folder, so the bubble still
// shows it after the original is moved or deleted.
func (c *Client) keep(path string, data []byte) string {
	dst := media.Unique(c.Dir, fmt.Sprintf("sent-%d%s", time.Now().UnixNano(), filepath.Ext(path)))
	if os.WriteFile(dst, data, 0o600) != nil {
		return path
	}
	return dst
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// sender is the JID a message key needs: the author as WhatsApp addressed them.
func (c *Client) sender(m *store.Message) types.JID {
	if m.FromMe {
		return c.Me()
	}
	if m.RawSender != "" {
		return jid(m.RawSender)
	}
	return jid(m.Sender)
}

// chatOf is the chat JID a message key needs.
func chatOf(m *store.Message) types.JID {
	if m.RawChat != "" {
		return jid(m.RawChat)
	}
	return jid(m.Chat)
}

// control sends a message about another message (reaction, edit, delete,
// vote) and applies it locally right away.
func (c *Client) control(m *store.Message, msg *waE2E.Message, apply func() error) {
	_ = apply()
	c.OnEvent(Event{Kind: EvUpdate, Chat: m.Chat, ID: m.ID})
	go func() {
		if _, err := c.WA.SendMessage(context.Background(), chatOf(m), msg); err != nil {
			fmt.Fprintln(os.Stderr, "send:", err)
		}
	}()
}

func (c *Client) React(m *store.Message, emoji string) {
	c.control(m, c.WA.BuildReaction(chatOf(m), c.sender(m), m.ID, emoji),
		func() error { return c.Store.React(m.Chat, m.ID, c.Me().String(), emoji) })
}

// Edit changes the text of one of our own messages; WhatsApp allows it for
// 15 minutes after sending.
func (c *Client) Edit(m *store.Message, text string) {
	c.control(m, c.WA.BuildEdit(chatOf(m), m.ID, &waE2E.Message{Conversation: proto.String(text)}),
		func() error { return c.Store.Edit(m.Chat, m.ID, text) })
}

// Revoke deletes a message for everyone.
func (c *Client) Revoke(m *store.Message) {
	c.control(m, c.WA.BuildRevoke(chatOf(m), c.sender(m), m.ID),
		func() error { return c.Store.Delete(m.Chat, m.ID) })
}

func (c *Client) Vote(poll *store.Message, options []string) error {
	chat := chatOf(poll)
	info := &types.MessageInfo{ID: poll.ID, MessageSource: types.MessageSource{
		Chat: chat, Sender: c.sender(poll), IsFromMe: poll.FromMe, IsGroup: chat.Server == types.GroupServer}}
	msg, err := c.WA.BuildPollVote(context.Background(), info, options)
	if err != nil {
		return err
	}
	c.control(poll, msg, func() error { return c.Store.Vote(poll.Chat, poll.ID, c.Me().String(), options) })
	return nil
}

// Typing tells the chat we are composing, or stopped.
func (c *Client) Typing(chat string, on bool) {
	state := types.ChatPresencePaused
	if on {
		state = types.ChatPresenceComposing
	}
	go c.WA.SendChatPresence(context.Background(), jid(chat), state, types.ChatPresenceMediaText)
}

// Online is set while the window is focused. While we are online the phone
// holds back its own notifications, as with WhatsApp Web.
func (c *Client) Online(on bool) {
	p := types.PresenceUnavailable
	if on {
		p = types.PresenceAvailable
	}
	go c.WA.SendPresence(context.Background(), p)
}
