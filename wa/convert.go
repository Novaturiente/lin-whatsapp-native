package wa

import (
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"lin-whatsapp-native/store"
)

// Update is what one incoming protocol message means for the store. Exactly
// one field is set; all nil means the message carries nothing to show
// (key distribution, app-state sync and similar plumbing).
type Update struct {
	Msg      *store.Message
	React    *React
	Edit     *Edit
	Revoke   *Target
	PollVote *Target // the vote itself is encrypted; Client decrypts it
	Timer    *uint32 // the chat's disappearing-messages setting changed to this many seconds

	// Expiration is the disappearing timer a message was sent with. Every
	// message in such a chat carries it, so it also repairs a missed Timer.
	Expiration uint32
}

type Target struct{ Chat, ID string }
type React struct {
	Target
	Sender, Emoji string
}
type Edit struct {
	Target
	Text string
}

// Convert turns a decrypted message into an Update. It knows nothing about
// the connection, so tests feed it hand-built protos. chat and sender are
// already normalised by the caller (phone-number JID over LID where known).
func Convert(evt *events.Message, chat, sender string) Update {
	m := evt.Message
	if m == nil {
		return Update{}
	}
	if r := m.GetReactionMessage(); r != nil {
		return Update{React: &React{Target{chat, r.GetKey().GetID()}, sender, r.GetText()}}
	}
	if p := m.GetProtocolMessage(); p != nil {
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return Update{Revoke: &Target{chat, p.GetKey().GetID()}}
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			return Update{Edit: &Edit{Target{chat, p.GetKey().GetID()}, Text(p.GetEditedMessage())}}
		case waE2E.ProtocolMessage_EPHEMERAL_SETTING:
			t := p.GetEphemeralExpiration()
			return Update{Timer: &t}
		}
		return Update{}
	}
	if m.GetPollUpdateMessage() != nil {
		return Update{PollVote: &Target{chat, m.GetPollUpdateMessage().GetPollCreationMessageKey().GetID()}}
	}

	out := &store.Message{
		Chat: chat, ID: evt.Info.ID, Sender: sender, FromMe: evt.Info.IsFromMe,
		Time: evt.Info.Timestamp, ViewOnce: evt.IsViewOnce, Status: store.StatusSent,
		Text: Text(m),
	}
	if out.Time.IsZero() {
		out.Time = time.Now()
	}
	switch {
	case m.GetConversation() != "" || m.GetExtendedTextMessage() != nil:
		out.Kind = store.KindText
		if x := m.GetExtendedTextMessage(); x.GetTitle() != "" || x.GetMatchedText() != "" {
			out.LinkURL, out.LinkTitle, out.LinkDesc = x.GetMatchedText(), x.GetTitle(), x.GetDescription()
			out.Thumb = x.GetJPEGThumbnail()
		}
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		out.Kind, out.Mime, out.Thumb = store.KindImage, x.GetMimetype(), x.GetJPEGThumbnail()
	case m.GetVideoMessage() != nil || m.GetPtvMessage() != nil:
		x := m.GetVideoMessage()
		if x == nil {
			x = m.GetPtvMessage()
		}
		out.Kind, out.Mime, out.Thumb = store.KindVideo, x.GetMimetype(), x.GetJPEGThumbnail()
		out.Animated = x.GetGifPlayback()
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		out.Kind, out.Mime, out.Voice = store.KindAudio, x.GetMimetype(), x.GetPTT()
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		out.Kind, out.Mime, out.Thumb = store.KindDocument, x.GetMimetype(), x.GetJPEGThumbnail()
		out.FileName = x.GetFileName()
		if out.FileName == "" {
			out.FileName = x.GetTitle()
		}
	case m.GetStickerMessage() != nil:
		x := m.GetStickerMessage()
		out.Kind, out.Mime, out.Animated = store.KindSticker, x.GetMimetype(), x.GetIsAnimated()
	case poll(m) != nil:
		p := poll(m)
		out.Kind, out.Text, out.MultiVote = store.KindPoll, p.GetName(), p.GetSelectableOptionsCount() != 1
		for _, o := range p.GetOptions() {
			out.Options = append(out.Options, o.GetOptionName())
		}
	case m.GetLocationMessage() != nil || m.GetLiveLocationMessage() != nil:
		out.Kind = store.KindLocation
		lat, lon := m.GetLocationMessage().GetDegreesLatitude(), m.GetLocationMessage().GetDegreesLongitude()
		name := m.GetLocationMessage().GetName()
		if l := m.GetLiveLocationMessage(); l != nil {
			lat, lon, name = l.GetDegreesLatitude(), l.GetDegreesLongitude(), "Live location"
		}
		out.Text = fmt.Sprintf("%s\nhttps://maps.google.com/?q=%f,%f", name, lat, lon)
	case m.GetContactMessage() != nil:
		out.Kind, out.Text = store.KindContact, m.GetContactMessage().GetDisplayName()
	case m.GetContactsArrayMessage() != nil:
		out.Kind = store.KindContact
		out.Text = fmt.Sprintf("%d contacts", len(m.GetContactsArrayMessage().GetContacts()))
	case m.GetGroupInviteMessage() != nil:
		x := m.GetGroupInviteMessage()
		out.Kind = store.KindText
		out.Text = fmt.Sprintf("Group invite: %s\nhttps://chat.whatsapp.com/%s", x.GetGroupName(), x.GetInviteCode())
	default:
		if !visible(m) {
			return Update{}
		}
		out.Kind = store.KindOther
	}
	if out.Kind == store.KindImage || out.Kind == store.KindVideo || out.Kind == store.KindAudio ||
		out.Kind == store.KindDocument || out.Kind == store.KindSticker {
		out.Raw, _ = proto.Marshal(m)
	}
	var expiration uint32
	if ci := contextInfo(m); ci != nil {
		expiration = ci.GetExpiration()
		out.Forwarded = ci.GetIsForwarded()
		out.Mentions = ci.GetMentionedJID()
		if ci.GetStanzaID() != "" {
			out.QuoteID, out.QuoteSender = ci.GetStanzaID(), ci.GetParticipant()
			q := &store.Message{Kind: store.KindText, Text: Text(ci.GetQuotedMessage())}
			if inner := Convert(&events.Message{Message: ci.GetQuotedMessage()}, "", ""); inner.Msg != nil {
				q = inner.Msg
			}
			out.QuoteText = store.Preview(q)
		}
	}
	if evt.IsViewOnce {
		out.Raw, out.Thumb = nil, nil // linked devices never get to open these
	}
	return Update{Msg: out, Expiration: expiration}
}

func poll(m *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{m.GetPollCreationMessage(),
		m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(), m.GetPollCreationMessageV4().GetMessage().GetPollCreationMessage()} {
		if p != nil {
			return p
		}
	}
	return nil
}

// Text is the user-written text of a message: body or caption.
func Text(m *waE2E.Message) string {
	switch {
	case m == nil:
		return ""
	case m.GetConversation() != "":
		return m.GetConversation()
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetCaption()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetCaption()
	}
	return ""
}

func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	for _, ci := range []*waE2E.ContextInfo{
		m.GetExtendedTextMessage().GetContextInfo(), m.GetImageMessage().GetContextInfo(),
		m.GetVideoMessage().GetContextInfo(), m.GetPtvMessage().GetContextInfo(),
		m.GetAudioMessage().GetContextInfo(), m.GetDocumentMessage().GetContextInfo(),
		m.GetStickerMessage().GetContextInfo(), m.GetLocationMessage().GetContextInfo(),
		m.GetContactMessage().GetContextInfo(),
	} {
		if ci != nil {
			return ci
		}
	}
	return nil
}

// visible reports whether an unknown message is something a person sent, as
// opposed to protocol plumbing that WhatsApp never shows either.
func visible(m *waE2E.Message) bool {
	plumbing := &waE2E.Message{
		SenderKeyDistributionMessage: m.GetSenderKeyDistributionMessage(),
		MessageContextInfo:           m.GetMessageContextInfo(),
		KeepInChatMessage:            m.GetKeepInChatMessage(),
		PinInChatMessage:             m.GetPinInChatMessage(),
		EncReactionMessage:           m.GetEncReactionMessage(),
		StickerSyncRmrMessage:        m.GetStickerSyncRmrMessage(),
		PlaceholderMessage:           m.GetPlaceholderMessage(),
		Call:                         m.GetCall(),
	}
	return proto.Size(m) > proto.Size(plumbing)
}
