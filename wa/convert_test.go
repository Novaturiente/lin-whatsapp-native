package wa

import (
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"lin-whatsapp-native/store"
)

const chat, sender = "91@s.whatsapp.net", "91@s.whatsapp.net"

func evt(m *waE2E.Message) *events.Message {
	return &events.Message{Message: m, Info: types.MessageInfo{ID: "ID1", Timestamp: time.Unix(100, 0)}}
}

func TestConvertKinds(t *testing.T) {
	quote := &waE2E.ContextInfo{StanzaID: proto.String("Q1"), Participant: proto.String("92@s.whatsapp.net"),
		QuotedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("beach")}},
		MentionedJID:  []string{"93@s.whatsapp.net"}}
	cases := []struct {
		name string
		in   *waE2E.Message
		kind string
		text string
		raw  bool
		chk  func(*store.Message) bool
	}{
		{"plain", &waE2E.Message{Conversation: proto.String("hi")}, store.KindText, "hi", false, nil},
		{"reply", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("nice @93"), ContextInfo: quote}}, store.KindText, "nice @93", false,
			func(m *store.Message) bool {
				return m.QuoteID == "Q1" && m.QuoteSender == "92@s.whatsapp.net" &&
					m.QuoteText == "📷 Photo: beach" && len(m.Mentions) == 1
			}},
		{"image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("c"),
			Mimetype: proto.String("image/jpeg"), JPEGThumbnail: []byte{1}}}, store.KindImage, "c", true,
			func(m *store.Message) bool { return len(m.Thumb) == 1 }},
		{"voice", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true),
			Mimetype: proto.String("audio/ogg; codecs=opus")}}, store.KindAudio, "", true,
			func(m *store.Message) bool { return m.Voice }},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			FileName: proto.String("a.pdf")}}, store.KindDocument, "", true,
			func(m *store.Message) bool { return m.FileName == "a.pdf" }},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsAnimated: proto.Bool(true),
			Mimetype: proto.String("image/webp")}}, store.KindSticker, "", true,
			func(m *store.Message) bool { return m.Animated }},
		{"poll", &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
			Name: proto.String("Lunch?"), SelectableOptionsCount: proto.Uint32(1),
			Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("yes")}, {OptionName: proto.String("no")}}}},
			store.KindPoll, "Lunch?", false,
			func(m *store.Message) bool { return !m.MultiVote && strings.Join(m.Options, ",") == "yes,no" }},
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude: proto.Float64(1.5), DegreesLongitude: proto.Float64(2)}}, store.KindLocation, "", false,
			func(m *store.Message) bool { return strings.Contains(m.Text, "q=1.500000,2.000000") }},
		{"unknown", &waE2E.Message{EventMessage: &waE2E.EventMessage{Name: proto.String("x")}}, store.KindOther, "", false, nil},
	}
	for _, c := range cases {
		u := Convert(evt(c.in), chat, sender)
		m := u.Msg
		if m == nil {
			t.Errorf("%s: no message", c.name)
			continue
		}
		if m.Kind != c.kind || (c.text != "" && m.Text != c.text) || (len(m.Raw) > 0) != c.raw ||
			(c.chk != nil && !c.chk(m)) || m.ID != "ID1" || m.Chat != chat {
			t.Errorf("%s: got %+v", c.name, m)
		}
	}
}

func TestConvertControl(t *testing.T) {
	key := &waCommon.MessageKey{ID: proto.String("T1")}
	u := Convert(evt(&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: key, Text: proto.String("👍")}}), chat, sender)
	if u.React == nil || u.React.ID != "T1" || u.React.Emoji != "👍" || u.React.Sender != sender {
		t.Errorf("reaction: %+v", u)
	}
	u = Convert(evt(&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: key,
		Type: waE2E.ProtocolMessage_REVOKE.Enum()}}), chat, sender)
	if u.Revoke == nil || u.Revoke.ID != "T1" {
		t.Errorf("revoke: %+v", u)
	}
	u = Convert(evt(&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: key,
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String("fixed")}}}), chat, sender)
	if u.Edit == nil || u.Edit.ID != "T1" || u.Edit.Text != "fixed" {
		t.Errorf("edit: %+v", u)
	}
	u = Convert(evt(&waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{PollCreationMessageKey: key}}), chat, sender)
	if u.PollVote == nil || u.PollVote.ID != "T1" {
		t.Errorf("poll vote: %+v", u)
	}
	u = Convert(evt(&waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String("g")}}), chat, sender)
	if u.Msg != nil {
		t.Errorf("plumbing shown: %+v", u.Msg)
	}
	e := evt(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{JPEGThumbnail: []byte{1}}})
	e.IsViewOnce = true
	if m := Convert(e, chat, sender).Msg; m == nil || !m.ViewOnce || m.Raw != nil || m.Thumb != nil {
		t.Errorf("view once kept media: %+v", m)
	}
}
