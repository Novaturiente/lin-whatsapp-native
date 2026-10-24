// Package wa connects to WhatsApp through whatsmeow and keeps the store in
// step with it. It knows nothing about GTK; the window listens to OnEvent.
package wa

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"lin-whatsapp-native/media"
	"lin-whatsapp-native/store"
)

type EventKind int

const (
	EvQR         EventKind = iota // QR holds the code to show
	EvConnected                   // logged in and online
	EvLoggedOut                   // the phone unlinked this device
	EvMessage                     // a new message in Chat; Notify set if it should alert
	EvUpdate                      // message ID in Chat changed: status, edit, delete, reaction, vote
	EvChats                       // chat list changed in bulk: history sync, names
	EvTyping                      // Typing tells whether Sender is composing in Chat
	EvError                       // Text says what failed, for the user
	EvConnection                  // Text is "" when online, else what the banner should say
	EvHistory                     // the phone sent older messages for Chat on request
	EvMissed                      // catch-up after being offline brought Text; Chat set if all in one chat
)

type Event struct {
	Kind     EventKind
	Chat, ID string
	Sender   string
	QR       string
	Typing   bool
	Text     string
	Notify   *store.Message
}

type Client struct {
	WA      *whatsmeow.Client
	Store   *store.Store
	Dir     string      // media files
	OnEvent func(Event) // called from whatsmeow's goroutines

	mu       sync.Mutex
	names    map[string]string
	asked    map[string]bool // groups whose name was requested
	fetching map[string][]func(string, error)
	outbox   map[string]*waE2E.Message // sends not yet accepted by the server, by ID

	// While the server replays what arrived during downtime, messages are
	// counted per chat instead of each raising a notification.
	catchingUp bool
	missed     map[string]int
}

// Open loads the linked device from dir, or prepares a new one to pair.
func Open(ctx context.Context, dir string, st *store.Store) (*Client, error) {
	container, err := sqlstore.New(ctx, "sqlite", "file:"+filepath.Join(dir, "wa.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		waLog.Stdout("db", "WARN", true))
	if err != nil {
		return nil, err
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	c := &Client{
		WA: whatsmeow.NewClient(device, waLog.Stdout("wa", "WARN", true)), Store: st,
		Dir: filepath.Join(dir, "media"), names: map[string]string{}, asked: map[string]bool{},
		fetching: map[string][]func(string, error){}, outbox: map[string]*waE2E.Message{},
		missed: map[string]int{},
	}
	c.WA.InitialAutoReconnect = true // a network blip at launch must not leave the app offline
	c.WA.AddEventHandler(c.handle)
	return c, os.MkdirAll(c.Dir, 0o700)
}

// Connect logs in, first pairing by QR code if this device is new.
func (c *Client) Connect(ctx context.Context) error {
	if c.WA.Store.ID != nil {
		return c.WA.Connect()
	}
	qr, err := c.WA.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := c.WA.Connect(); err != nil {
		return err
	}
	go func() {
		for item := range qr {
			if item.Event == whatsmeow.QRChannelEventCode {
				c.OnEvent(Event{Kind: EvQR, QR: item.Code})
			}
		}
	}()
	return nil
}

func (c *Client) Me() types.JID {
	if c.WA.Store.ID == nil {
		return types.EmptyJID
	}
	return c.WA.Store.ID.ToNonAD()
}

// normalize gives one JID per person: WhatsApp addresses some chats by a
// hidden LID and others by phone number, which would split them in two.
func (c *Client) normalize(ctx context.Context, j types.JID) types.JID {
	j = j.ToNonAD()
	if j.Server == types.HiddenUserServer {
		if pn, err := c.WA.Store.LIDs.GetPNForLID(ctx, j); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return j
}

func (c *Client) handle(raw any) {
	ctx := context.Background()
	if c.appState(ctx, raw) {
		return
	}
	switch e := raw.(type) {
	case *events.Message:
		c.message(ctx, e, true)
	case *events.HistorySync:
		c.history(ctx, e)
	case *events.Receipt:
		c.receipt(ctx, e)
	case *events.ChatPresence:
		c.OnEvent(Event{Kind: EvTyping, Chat: c.normalize(ctx, e.Chat).String(),
			Sender: c.normalize(ctx, e.Sender).String(), Typing: e.State == types.ChatPresenceComposing})
	case *events.Connected:
		_ = c.WA.SendPresence(ctx, types.PresenceUnavailable) // the window says when it is looked at
		c.fillFlags(ctx)
		c.OnEvent(Event{Kind: EvConnected})
		c.OnEvent(Event{Kind: EvConnection})
	case *events.KeepAliveRestored:
		c.OnEvent(Event{Kind: EvConnection})
	case *events.OfflineSyncPreview:
		if e.Messages > 0 {
			c.mu.Lock()
			c.catchingUp = true
			c.mu.Unlock()
		}
	case *events.OfflineSyncCompleted:
		c.caughtUp()
	case *events.Disconnected:
		c.mu.Lock()
		c.catchingUp = false // a cut-off catch-up must not mute live messages; counts carry to the next one
		c.mu.Unlock()
		c.OnEvent(Event{Kind: EvConnection, Text: "Connecting…"})
	case *events.KeepAliveTimeout:
		c.OnEvent(Event{Kind: EvConnection, Text: "Connecting…"})
	case *events.StreamReplaced:
		c.OnEvent(Event{Kind: EvConnection, Text: "WhatsApp was opened on another computer. Restart to use it here."})
	case *events.ConnectFailure:
		c.OnEvent(Event{Kind: EvConnection, Text: "Could not connect: " + e.Reason.String()})
	case *events.TemporaryBan:
		c.OnEvent(Event{Kind: EvConnection, Text: "WhatsApp has temporarily banned this account: " + e.String()})
	case *events.LoggedOut:
		c.OnEvent(Event{Kind: EvConnection, Text: "This device was unlinked. Restart the app to link it again."})
		c.OnEvent(Event{Kind: EvLoggedOut})
	case *events.PushName, *events.Contact, *events.BusinessName:
		c.forgetNames()
	case *events.GroupInfo:
		c.groupChange(ctx, e)
	case *events.JoinedGroup:
		_ = c.Store.SetChatName(e.JID.String(), e.GroupInfo.Name)
		_ = c.Store.SetEphemeral(e.JID.String(), e.GroupInfo.DisappearingTimer)
		c.forgetNames()
	case *events.MarkChatAsRead:
		if e.Action.GetRead() {
			_ = c.Store.SetUnread(c.normalize(ctx, e.JID).String(), 0)
			c.OnEvent(Event{Kind: EvChats})
		}
	}
}

// caughtUp ends a catch-up with one summary instead of a notification per message.
func (c *Client) caughtUp() {
	c.mu.Lock()
	missed := c.missed
	c.catchingUp, c.missed = false, map[string]int{}
	c.mu.Unlock()
	n, only := 0, ""
	for chat, k := range missed {
		n, only = n+k, chat
	}
	if n == 0 {
		return
	}
	ev := Event{Kind: EvMissed, Text: plural(n, "new message")}
	if len(missed) == 1 {
		ev.Chat = only
	} else {
		ev.Text += " in " + plural(len(missed), "chat")
	}
	c.OnEvent(ev)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (c *Client) forgetNames() {
	c.mu.Lock()
	c.names = map[string]string{}
	c.mu.Unlock()
	c.OnEvent(Event{Kind: EvChats})
}

// skip is true for chats this client does not show: status updates,
// channels and our own broadcast-list sends.
func skip(chat types.JID) bool {
	return chat == types.StatusBroadcastJID || chat.Server == types.NewsletterServer ||
		chat.Server == types.BroadcastServer
}

func (c *Client) message(ctx context.Context, e *events.Message, live bool) {
	if skip(e.Info.Chat) {
		return
	}
	chat := c.normalize(ctx, e.Info.Chat).String()
	sender := c.normalize(ctx, e.Info.Sender).String()
	if e.Info.IsFromMe {
		sender = c.Me().String()
	}
	u := Convert(e, chat, sender)
	if live && u.Expiration > 0 { // a new message is proof of the chat's current timer; an old one is not
		if ch, _ := c.Store.Chat(chat); ch.Ephemeral != u.Expiration {
			_ = c.Store.SetEphemeral(chat, u.Expiration)
		}
	}
	var target string
	switch {
	case u.Timer != nil: // kept silently: sends must carry the timer the chat uses
		_ = c.Store.SetEphemeral(chat, *u.Timer)
		return
	case u.Msg != nil:
		u.Msg.RawChat, u.Msg.RawSender = e.Info.Chat.ToNonAD().String(), e.Info.Sender.ToNonAD().String()
		if e.SourceWebMsg != nil && u.Msg.FromMe {
			u.Msg.Status = webStatus(e.SourceWebMsg.GetStatus())
		}
		if err := c.Store.AddMessage(u.Msg, live); err != nil {
			fmt.Fprintln(os.Stderr, "store:", err)
			return
		}
		if live {
			ev := Event{Kind: EvMessage, Chat: chat, ID: u.Msg.ID}
			if !u.Msg.FromMe {
				c.mu.Lock()
				if c.catchingUp {
					if ch, _ := c.Store.Chat(chat); !ch.Muted() {
						c.missed[chat]++
					}
				} else {
					ev.Notify = u.Msg
				}
				c.mu.Unlock()
			}
			c.OnEvent(ev)
		}
		return
	case u.React != nil:
		_, target = c.Store.React(chat, u.React.ID, sender, u.React.Emoji), u.React.ID
	case u.Edit != nil:
		if !c.ownedBy(chat, u.Edit.ID, sender) {
			return
		}
		_, target = c.Store.Edit(chat, u.Edit.ID, u.Edit.Text), u.Edit.ID
	case u.Revoke != nil:
		if !c.ownedBy(chat, u.Revoke.ID, sender) {
			return
		}
		_, target = c.Store.Delete(chat, u.Revoke.ID), u.Revoke.ID
	case u.PollVote != nil:
		target = u.PollVote.ID
		if err := c.vote(ctx, e, chat, sender, target); err != nil {
			fmt.Fprintln(os.Stderr, "poll vote:", err)
			return
		}
	case u.Sealed != nil:
		target = u.Sealed.ID
		if !c.unseal(ctx, e, chat, sender, target) {
			return
		}
	default:
		return
	}
	if live {
		c.OnEvent(Event{Kind: EvUpdate, Chat: chat, ID: target})
	}
}

// unseal decrypts an edit or reaction sealed with the target's secret and
// applies it. It reports whether anything changed.
func (c *Client) unseal(ctx context.Context, e *events.Message, chat, sender, target string) bool {
	if e.Message.GetEncReactionMessage() != nil {
		r, err := c.WA.DecryptReaction(ctx, e)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reaction:", err)
			return false
		}
		return c.Store.React(chat, target, sender, r.GetText()) == nil
	}
	inner, err := c.WA.DecryptSecretEncryptedMessage(ctx, e)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sealed message:", err)
		return false
	}
	if p := inner.GetProtocolMessage(); p != nil && p.GetEditedMessage() != nil {
		inner = p.GetEditedMessage()
	}
	if !c.ownedBy(chat, target, sender) {
		return false
	}
	return c.Store.Edit(chat, target, Text(inner)) == nil
}

// ownedBy reports whether sender wrote the message, so only its author can
// edit or delete it here.
// shortcut: a group admin deleting someone else's message is ignored; check
// admin rights through GetGroupInfo if that matters.
func (c *Client) ownedBy(chat, id, sender string) bool {
	m, err := c.Store.Message(chat, id)
	return err == nil && m != nil && m.Sender == sender
}

func (c *Client) vote(ctx context.Context, e *events.Message, chat, sender, pollID string) error {
	v, err := c.WA.DecryptPollVote(ctx, e)
	if err != nil {
		return err
	}
	return c.storeVote(chat, pollID, sender, v)
}

func (c *Client) storeVote(chat, pollID, sender string, v *waE2E.PollVoteMessage) error {
	poll, err := c.Store.Message(chat, pollID)
	if err != nil || poll == nil {
		return fmt.Errorf("poll %s not stored: %v", pollID, err)
	}
	hashes := whatsmeow.HashPollOptions(poll.Options)
	var chosen []string
	for _, sel := range v.GetSelectedOptions() {
		for i, h := range hashes {
			if bytes.Equal(sel, h) {
				chosen = append(chosen, poll.Options[i])
			}
		}
	}
	return c.Store.Vote(chat, pollID, sender, chosen)
}

func webStatus(s waWeb.WebMessageInfo_Status) int {
	switch s {
	case waWeb.WebMessageInfo_ERROR:
		return store.StatusFailed
	case waWeb.WebMessageInfo_PENDING:
		return store.StatusPending
	case waWeb.WebMessageInfo_DELIVERY_ACK:
		return store.StatusDelivered
	case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
		return store.StatusRead
	}
	return store.StatusSent
}

// history stores the old messages the phone sends after pairing, and later
// in smaller batches. They never notify and never count as unread; the
// conversation's own unread count is taken instead.
func (c *Client) history(ctx context.Context, e *events.HistorySync) {
	for _, conv := range e.Data.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil || skip(jid) {
			continue
		}
		chat := c.normalize(ctx, jid).String()
		_ = c.Store.SetChatName(chat, conv.GetName())
		// Only a full sync states the timer; on-demand batches leave it out.
		if conv.EphemeralExpiration != nil && e.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
			_ = c.Store.SetEphemeral(chat, conv.GetEphemeralExpiration())
		}
		// History arrives newest first; store oldest first so edits and
		// deletes find the message they refer to.
		msgs := conv.GetMessages()
		for i := len(msgs) - 1; i >= 0; i-- {
			wm := msgs[i].GetMessage()
			if wm.GetMessageStubType() != 0 {
				if text := c.stubText(ctx, wm); text != "" {
					c.system(chat, wm.GetKey().GetID(), time.Unix(int64(wm.GetMessageTimestamp()), 0), text, false)
				}
				continue
			}
			evt, err := c.WA.ParseWebMessage(jid, wm)
			if err != nil {
				continue
			}
			c.message(ctx, evt, false)
			id := wm.GetKey().GetID()
			if wm.GetStarred() { // a Star event may have come before the message was stored
				_ = c.Store.SetStarred(chat, id, true)
			}
			for _, r := range wm.GetReactions() {
				_ = c.Store.React(chat, id, c.keySender(ctx, r.GetKey().GetFromMe(), r.GetKey().GetParticipant(), r.GetKey().GetRemoteJID()), r.GetText())
			}
			for _, p := range wm.GetPollUpdates() {
				k := p.GetPollUpdateMessageKey()
				_ = c.storeVote(chat, id, c.keySender(ctx, k.GetFromMe(), k.GetParticipant(), k.GetRemoteJID()), p.GetVote())
			}
		}
		if n := conv.GetUnreadCount(); n > 0 {
			_ = c.Store.SetUnread(chat, int(n))
		}
		// after the messages: flags never create a chat row
		c.historyFlags(chat, conv.GetArchived(), conv.GetPinned(), conv.GetMuteEndTime())
		if e.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND {
			c.OnEvent(Event{Kind: EvHistory, Chat: chat})
		}
	}
	c.OnEvent(Event{Kind: EvChats})
}

// RequestOlder asks the phone for up to 50 messages older than oldest. The
// answer comes back as a history sync, then EvHistory.
func (c *Client) RequestOlder(oldest *store.Message) {
	info := &types.MessageInfo{ID: oldest.ID, Timestamp: oldest.Time,
		MessageSource: types.MessageSource{Chat: chatOf(oldest), Sender: c.sender(oldest), IsFromMe: oldest.FromMe}}
	go func() {
		_, err := c.WA.SendMessage(context.Background(), c.Me(), c.WA.BuildHistorySyncRequest(info, 50),
			whatsmeow.SendRequestExtra{Peer: true})
		if err != nil {
			fmt.Fprintln(os.Stderr, "history request:", err)
		}
	}()
}

// system stores a grey notice line in a chat; live ones show at once. It
// reports whether the line is new, since notifications repeat on reconnect.
func (c *Client) system(chat, id string, at time.Time, text string, live bool) bool {
	if at.IsZero() {
		at = time.Now()
	}
	if old, _ := c.Store.Message(chat, id); old != nil {
		return false
	}
	m := &store.Message{Chat: chat, ID: id, Sender: chat, Time: at, Kind: store.KindSystem,
		Text: text, Status: store.StatusSent}
	if err := c.Store.AddMessage(m, false); err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		return false
	}
	if live {
		c.OnEvent(Event{Kind: EvMessage, Chat: chat, ID: id})
	}
	return true
}

// groupChange turns a group notification into notice lines, and keeps the
// stored name and timer current.
func (c *Client) groupChange(ctx context.Context, e *events.GroupInfo) {
	chat := e.JID.String()
	who := "Someone"
	if e.SenderPN != nil {
		who = c.Name(e.SenderPN.String())
	} else if e.Sender != nil {
		who = c.Name(c.normalize(ctx, *e.Sender).String())
	}
	names := func(list []types.JID) string {
		var out []string
		for _, j := range list {
			out = append(out, c.Name(c.normalize(ctx, j).String()))
		}
		return strings.Join(out, ", ")
	}
	var lines []string
	if e.Name != nil {
		_ = c.Store.SetChatName(chat, e.Name.Name)
		c.forgetNames()
		lines = append(lines, fmt.Sprintf("%s changed the group name to \"%s\"", who, e.Name.Name))
	}
	if e.Topic != nil {
		lines = append(lines, who+" changed the group description")
	}
	if e.Ephemeral != nil {
		t := e.Ephemeral.DisappearingTimer
		if !e.Ephemeral.IsEphemeral {
			t = 0
		}
		_ = c.Store.SetEphemeral(chat, t)
	}
	if e.Announce != nil {
		if e.Announce.IsAnnounce {
			lines = append(lines, who+" changed settings so only admins can send messages")
		} else {
			lines = append(lines, who+" changed settings so all members can send messages")
		}
	}
	if e.Locked != nil {
		if e.Locked.IsLocked {
			lines = append(lines, who+" changed settings so only admins can edit group info")
		} else {
			lines = append(lines, who+" changed settings so all members can edit group info")
		}
	}
	if len(e.Join) > 0 {
		if e.Sender != nil && len(e.Join) == 1 && c.normalize(ctx, e.Join[0]) == c.normalize(ctx, *e.Sender) {
			lines = append(lines, names(e.Join)+" joined")
		} else {
			lines = append(lines, who+" added "+names(e.Join))
		}
	}
	if len(e.Leave) > 0 {
		if e.Sender != nil && len(e.Leave) == 1 && c.normalize(ctx, e.Leave[0]) == c.normalize(ctx, *e.Sender) {
			lines = append(lines, names(e.Leave)+" left")
		} else {
			lines = append(lines, who+" removed "+names(e.Leave))
		}
	}
	if len(e.Promote) > 0 {
		lines = append(lines, names(e.Promote)+" is now an admin")
	}
	if len(e.Demote) > 0 {
		lines = append(lines, names(e.Demote)+" is no longer an admin")
	}
	for i, text := range lines {
		// timestamps are whole seconds; the text keeps two changes in one second apart
		h := fnv.New32a()
		h.Write([]byte(text))
		c.system(chat, fmt.Sprintf("sys-%d-%d-%x", e.Timestamp.Unix(), i, h.Sum32()), e.Timestamp, text, true)
	}
}

// stubText renders the notice lines history sync carries as stub types.
func (c *Client) stubText(ctx context.Context, wm *waWeb.WebMessageInfo) string {
	var people []string
	for _, p := range wm.GetMessageStubParameters() {
		if j, err := types.ParseJID(p); err == nil && j.User != "" && j.Server != "" {
			people = append(people, c.Name(c.normalize(ctx, j).String()))
		}
	}
	who := "Someone"
	if p := wm.GetParticipant(); p != "" {
		who = c.Name(c.keySender(ctx, false, p, ""))
	} else if wm.GetKey().GetFromMe() {
		who = "You"
	} else if r := wm.GetKey().GetRemoteJID(); !strings.HasSuffix(r, "@g.us") {
		who = c.Name(c.keySender(ctx, false, "", r)) // a direct chat: the other person
	}
	params := wm.GetMessageStubParameters()
	first := ""
	if len(params) > 0 {
		first = params[0]
	}
	list := strings.Join(people, ", ")
	switch wm.GetMessageStubType() {
	case waWeb.WebMessageInfo_GROUP_CREATE:
		return fmt.Sprintf("%s created group \"%s\"", who, first)
	case waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT:
		return fmt.Sprintf("%s changed the group name to \"%s\"", who, first)
	case waWeb.WebMessageInfo_GROUP_CHANGE_DESCRIPTION:
		return who + " changed the group description"
	case waWeb.WebMessageInfo_GROUP_CHANGE_ICON:
		return who + " changed this group's icon"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD, waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE:
		return who + " added " + list
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE:
		return who + " removed " + list
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE:
		return list + " left"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_PROMOTE:
		return list + " is now an admin"
	case waWeb.WebMessageInfo_GROUP_PARTICIPANT_DEMOTE:
		return list + " is no longer an admin"
	}
	return "" // calls and timer changes are not shown, by choice
}

// keySender is who a message key says sent something, normalised.
func (c *Client) keySender(ctx context.Context, fromMe bool, participant, remote string) string {
	if fromMe {
		return c.Me().String()
	}
	if participant == "" {
		participant = remote
	}
	j, err := types.ParseJID(participant)
	if err != nil {
		return participant
	}
	return c.normalize(ctx, j).String()
}

// receipt moves our sent messages along: sent, delivered, read.
// shortcut: in groups the first member's receipt sets the tick, where
// WhatsApp waits for all of them; track per member if it misleads.
func (c *Client) receipt(ctx context.Context, e *events.Receipt) {
	chat := c.normalize(ctx, e.Chat).String()
	if e.Type == types.ReceiptTypeReadSelf { // read on the phone
		_ = c.Store.SetUnread(chat, 0)
		c.OnEvent(Event{Kind: EvChats})
		return
	}
	status := map[types.ReceiptType]int{
		types.ReceiptTypeDelivered: store.StatusDelivered,
		types.ReceiptTypeRead:      store.StatusRead,
		types.ReceiptTypePlayed:    store.StatusRead,
	}[e.Type]
	if status == 0 {
		return
	}
	member := c.normalize(ctx, e.Sender).String()
	for _, id := range e.MessageIDs {
		if !e.IsFromMe { // from the person it was sent to, for Message info
			_ = c.Store.AddReceipt(chat, id, member, status, e.Timestamp)
		}
		_ = c.Store.SetStatus(chat, id, status)
		c.OnEvent(Event{Kind: EvUpdate, Chat: chat, ID: id})
	}
}

// Name is how a person or group is shown: the phone's contact name, then
// their own push name, then the number.
func (c *Client) Name(jid string) string {
	c.mu.Lock()
	n, ok := c.names[jid]
	c.mu.Unlock()
	if ok {
		return n
	}
	j, err := types.ParseJID(jid)
	if err != nil {
		return jid
	}
	ctx := context.Background()
	j = c.normalize(ctx, j)
	switch {
	case j.User == c.Me().User:
		n = "You"
	case j.Server == types.GroupServer:
		n = c.groupName(ctx, j)
	default:
		info, _ := c.WA.Store.Contacts.GetContact(ctx, j)
		for _, v := range []string{info.FullName, info.FirstName, info.PushName, info.BusinessName} {
			if v != "" {
				n = v
				break
			}
		}
		if n == "" && j.Server == types.DefaultUserServer {
			n = "+" + j.User
		} else if n == "" {
			n = info.RedactedPhone
		}
		if n == "" {
			n = j.User
		}
	}
	if n != "Group" { // a placeholder must not stick
		c.mu.Lock()
		c.names[jid] = n
		c.mu.Unlock()
	}
	return n
}

// groupName answers from the store; an unknown group is looked up in the
// background, because Name runs on the UI thread.
func (c *Client) groupName(ctx context.Context, j types.JID) string {
	chats, _ := c.Store.Chats()
	for _, ch := range chats {
		if ch.JID == j.String() && ch.Name != "" {
			return ch.Name
		}
	}
	c.mu.Lock()
	asked := c.asked[j.String()]
	c.asked[j.String()] = true
	c.mu.Unlock()
	if asked { // once per run; a group we left never answers
		return "Group"
	}
	go func() {
		if info, err := c.WA.GetGroupInfo(ctx, j); err == nil && info.Name != "" {
			_ = c.Store.SetChatName(j.String(), info.Name)
			c.forgetNames()
		}
	}()
	return "Group"
}

type Member struct {
	JID   string // by phone number where known
	Admin bool
}

type Group struct {
	Name, Topic string
	Created     time.Time
	Members     []Member // ourselves included
}

// Group asks the server for a group's details. It blocks: call it off the UI thread.
func (c *Client) Group(chat string) (*Group, error) {
	j, err := types.ParseJID(chat)
	if err != nil || j.Server != types.GroupServer {
		return nil, fmt.Errorf("not a group")
	}
	ctx := context.Background()
	info, err := c.WA.GetGroupInfo(ctx, j)
	if err != nil {
		return nil, err
	}
	g := &Group{Name: info.Name, Topic: info.Topic, Created: info.GroupCreated}
	for _, p := range info.Participants {
		pj := p.JID
		if !p.PhoneNumber.IsEmpty() {
			pj = p.PhoneNumber
		}
		g.Members = append(g.Members, Member{c.normalize(ctx, pj).String(), p.IsAdmin || p.IsSuperAdmin})
	}
	return g, nil
}

// Members lists a group's participants for @mentions, by phone-number JID.
func (c *Client) Members(chat string) []string {
	g, err := c.Group(chat)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range g.Members {
		if jid(m.JID).User != c.Me().User {
			out = append(out, m.JID)
		}
	}
	return out
}

type Contact struct{ JID, Name string }

// Contacts lists people from the phone's address book plus anyone whose
// push name we have seen, by name, for starting a chat.
func (c *Client) Contacts() []Contact {
	ctx := context.Background()
	all, err := c.WA.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Contact
	for j := range all {
		pn := c.normalize(ctx, j)
		if pn.Server != types.DefaultUserServer || pn.User == c.Me().User || seen[pn.String()] {
			continue
		}
		seen[pn.String()] = true
		out = append(out, Contact{pn.String(), c.Name(pn.String())})
	}
	sort.Slice(out, func(a, b int) bool { return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name) })
	return out
}

// Lookup checks a phone number (any format, with country code) is on
// WhatsApp and returns its chat JID.
func (c *Client) Lookup(phone string) (string, error) {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	if len(digits) < 8 {
		return "", fmt.Errorf("enter the full number with country code")
	}
	res, err := c.WA.IsOnWhatsApp(context.Background(), []string{"+" + digits})
	if err != nil {
		return "", err
	}
	if len(res) == 0 || !res[0].IsIn {
		return "", fmt.Errorf("+%s is not on WhatsApp", digits)
	}
	return res[0].JID.ToNonAD().String(), nil
}

// MarkRead sends read receipts for a chat's unread messages.
func (c *Client) MarkRead(chat string) {
	chats, _ := c.Store.Chats()
	unread := 0
	for _, ch := range chats {
		if ch.JID == chat {
			unread = ch.Unread
		}
	}
	if unread == 0 {
		return
	}
	_ = c.Store.SetUnread(chat, 0)
	msgs, _ := c.Store.Unread(chat, unread)
	// One receipt per address the messages came in on, as WhatsApp keyed them.
	type addr struct{ chat, sender string }
	by := map[addr][]string{}
	for _, m := range msgs {
		if m.RawChat == "" { // sent before raw JIDs were kept
			m.RawChat, m.RawSender = m.Chat, m.Sender
		}
		a := addr{m.RawChat, m.RawSender}
		by[a] = append(by[a], m.ID)
	}
	for a, ids := range by {
		cj, sj := jid(a.chat), jid(a.sender)
		if cj.Server != types.GroupServer {
			sj = types.EmptyJID
		}
		go c.WA.MarkRead(context.Background(), ids, time.Now(), cj, sj)
	}
}

// Fetch downloads a message's media once and calls done with its path. Files
// are named by content hash, so a sticker seen in ten chats is stored once.
// done runs on a background goroutine.
func (c *Client) Fetch(m *store.Message, done func(string, error)) {
	if m.Media != "" {
		if _, err := os.Stat(m.Media); err == nil {
			done(m.Media, nil)
			return
		}
	}
	if len(m.Raw) == 0 {
		done("", fmt.Errorf("no media"))
		return
	}
	key := m.Chat + "/" + m.ID
	c.mu.Lock()
	waiting, busy := c.fetching[key]
	c.fetching[key] = append(waiting, done)
	c.mu.Unlock()
	if busy {
		return
	}
	go func() {
		path, err := c.download(m)
		if err == nil {
			_ = c.Store.SetMedia(m.Chat, m.ID, path)
		}
		c.mu.Lock()
		list := c.fetching[key]
		delete(c.fetching, key)
		c.mu.Unlock()
		for _, f := range list {
			f(path, err)
		}
	}()
}

func (c *Client) download(m *store.Message) (string, error) {
	var msg waE2E.Message
	if err := proto.Unmarshal(m.Raw, &msg); err != nil {
		return "", err
	}
	data, err := c.WA.DownloadAny(context.Background(), &msg)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(c.Dir, hex.EncodeToString(sum[:16])+media.Ext(m.Mime, m.FileName))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	tmp, err := os.CreateTemp(c.Dir, "*.part") // the same sticker may download twice at once
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	return path, os.Rename(tmp.Name(), path)
}

// Avatar downloads a chat's profile picture thumbnail, or returns "".
func (c *Client) Avatar(chat string) string {
	j, err := types.ParseJID(chat)
	if err != nil {
		return ""
	}
	path := filepath.Join(c.Dir, "avatar-"+strings.ReplaceAll(j.User, ":", "_")+".jpg")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	info, err := c.WA.GetProfilePictureInfo(context.Background(), j, &whatsmeow.GetProfilePictureParams{Preview: true})
	if err != nil || info == nil {
		return ""
	}
	data, err := media.Get(info.URL) // plain HTTPS, not encrypted media
	if err != nil || os.WriteFile(path, data, 0o600) != nil {
		return ""
	}
	return path
}
