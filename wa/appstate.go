package wa

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"lin-whatsapp-native/store"
)

// Chat and message flags (archive, pin, mute, star) are app state: synced
// between the phone and every linked device. Changes made here apply to the
// store at once and are then sent; changes made elsewhere arrive as events.

// appState applies an app state event from another device. It reports
// whether the event was one of these.
func (c *Client) appState(ctx context.Context, raw any) bool {
	switch e := raw.(type) {
	case *events.Archive:
		_ = c.Store.SetArchived(c.normalize(ctx, e.JID).String(), e.Action.GetArchived())
	case *events.Pin:
		var at int64
		if e.Action.GetPinned() {
			at = e.Timestamp.UnixMilli()
		}
		_ = c.Store.SetPinned(c.normalize(ctx, e.JID).String(), at)
	case *events.Mute:
		var until int64
		if e.Action.GetMuted() {
			until = e.Action.GetMuteEndTimestamp()
			if until <= 0 {
				until = -1
			}
		}
		_ = c.Store.SetMuted(c.normalize(ctx, e.JID).String(), until)
	case *events.Star:
		chat := c.normalize(ctx, e.ChatJID).String()
		_ = c.Store.SetStarred(chat, e.MessageID, e.Action.GetStarred())
		c.OnEvent(Event{Kind: EvUpdate, Chat: chat, ID: e.MessageID})
		return true
	default:
		return false
	}
	c.OnEvent(Event{Kind: EvChats})
	return true
}

// historyFlags takes archive, pin and mute from a history sync conversation.
// Its pin and mute times are in seconds. Only set flags are applied: later
// batches leave them out, and must not undo what app state said.
func (c *Client) historyFlags(chat string, archived bool, pinned uint32, muteEnd uint64) {
	if archived {
		_ = c.Store.SetArchived(chat, true)
	}
	if pinned > 0 {
		_ = c.Store.SetPinned(chat, int64(pinned)*1000)
	}
	if until := int64(muteEnd); until < 0 { // all bits set: muted for good
		_ = c.Store.SetMuted(chat, -1)
	} else if until > 0 {
		_ = c.Store.SetMuted(chat, until*1000)
	}
}

// lastReal is a chat's newest message that is not a notice line.
func (c *Client) lastReal(chat string) *store.Message {
	page, _ := c.Store.Messages(chat, nil, 20)
	for i := len(page) - 1; i >= 0; i-- {
		if page[i].Kind != store.KindSystem {
			return page[i]
		}
	}
	return nil
}

// address is the JID WhatsApp keys a chat by: as its newest message came
// in (often a hidden LID), else the stored one.
func (c *Client) address(chat string) types.JID {
	if m := c.lastReal(chat); m != nil {
		return chatOf(m)
	}
	return jid(chat)
}

// fillFlags copies archive, pin and mute from whatsmeow's own record of app
// state into chats that have none yet: whatsmeow does not replay app state
// it already synced, so chats stored before these flags existed lack them.
func (c *Client) fillFlags(ctx context.Context) {
	chats, _ := c.Store.Chats()
	changed := false
	for _, ch := range chats {
		if ch.Archived || ch.Pinned != 0 || ch.MutedUntil != 0 {
			continue
		}
		for _, j := range []types.JID{jid(ch.JID), c.address(ch.JID)} {
			s, err := c.WA.Store.ChatSettings.GetChatSettings(ctx, j)
			if err != nil || !s.Found {
				continue
			}
			if s.Archived {
				_ = c.Store.SetArchived(ch.JID, true)
				changed = true
			} else if s.Pinned {
				_ = c.Store.SetPinned(ch.JID, 1) // the pin time is not kept
				changed = true
			}
			// muted for good is the year 9999 here, which works as a plain end time
			if until := s.MutedUntil.UnixMilli(); until > time.Now().UnixMilli() {
				_ = c.Store.SetMuted(ch.JID, until)
				changed = true
			}
			break
		}
	}
	if changed {
		c.OnEvent(Event{Kind: EvChats})
	}
}

// sendPatch sends an app state change in the background and reports failure.
func (c *Client) sendPatch(what string, p appstate.PatchInfo) {
	go func() {
		if err := c.WA.SendAppState(context.Background(), p); err != nil {
			fmt.Fprintln(os.Stderr, what+":", err)
			c.OnEvent(Event{Kind: EvError, Text: "Could not " + what + " on your phone: " + err.Error()})
		}
	}()
}

// Archive archives or unarchives a chat.
func (c *Client) Archive(chat string, on bool) {
	wasPinned := false
	if ch, _ := c.Store.Chat(chat); ch.Pinned != 0 {
		wasPinned = true
	}
	_ = c.Store.SetArchived(chat, on)
	c.OnEvent(Event{Kind: EvChats})
	if on && wasPinned { // archiving unpins here; say so to the other devices
		c.sendPatch("unpin the chat", appstate.BuildPin(c.address(chat), false))
	}
	var at time.Time
	var key *waCommon.MessageKey
	if m := c.lastReal(chat); m != nil {
		at = m.Time
		key = &waCommon.MessageKey{RemoteJID: proto.String(chatOf(m).String()),
			FromMe: proto.Bool(m.FromMe), ID: proto.String(m.ID)}
		if chatOf(m).Server == types.GroupServer && !m.FromMe {
			key.Participant = proto.String(c.sender(m).String())
		}
	}
	c.sendPatch("archive the chat", appstate.BuildArchive(c.address(chat), on, at, key))
}

// Pin pins or unpins a chat.
func (c *Client) Pin(chat string, on bool) {
	var at int64
	if on {
		at = time.Now().UnixMilli()
	}
	_ = c.Store.SetPinned(chat, at)
	c.OnEvent(Event{Kind: EvChats})
	c.sendPatch("pin the chat", appstate.BuildPin(c.address(chat), on))
}

// Mute silences a chat for d, for good when d is 0, or unmutes it when on is false.
func (c *Client) Mute(chat string, on bool, d time.Duration) {
	var until int64
	switch {
	case on && d > 0:
		until = time.Now().Add(d).UnixMilli()
	case on:
		until = -1
	}
	_ = c.Store.SetMuted(chat, until)
	c.OnEvent(Event{Kind: EvChats})
	c.sendPatch("mute the chat", appstate.BuildMute(c.address(chat), on, d))
}

// Star stars or unstars a message.
func (c *Client) Star(m *store.Message, on bool) {
	_ = c.Store.SetStarred(m.Chat, m.ID, on)
	c.OnEvent(Event{Kind: EvUpdate, Chat: m.Chat, ID: m.ID})
	// The key names the author only for others' messages in groups.
	sender := chatOf(m)
	if sender.Server == types.GroupServer && !m.FromMe {
		sender = c.sender(m)
	}
	c.sendPatch("star the message", appstate.BuildStar(chatOf(m), sender, m.ID, m.FromMe, on))
}
