package wa

import "testing"

func TestCaughtUp(t *testing.T) {
	cases := []struct {
		missed     map[string]int
		text, chat string
		fires      bool
	}{
		{map[string]int{}, "", "", false},
		{map[string]int{"a": 1}, "1 new message", "a", true},
		{map[string]int{"a": 3}, "3 new messages", "a", true},
		{map[string]int{"a": 2, "b": 1}, "3 new messages in 2 chats", "", true},
	}
	for _, tc := range cases {
		var got []Event
		c := &Client{catchingUp: true, missed: tc.missed, OnEvent: func(e Event) { got = append(got, e) }}
		c.caughtUp()
		if c.catchingUp || len(c.missed) != 0 {
			t.Errorf("%v: state not reset", tc.missed)
		}
		if (len(got) == 1) != tc.fires {
			t.Fatalf("%v: got %d events", tc.missed, len(got))
		}
		if tc.fires && (got[0].Kind != EvMissed || got[0].Text != tc.text || got[0].Chat != tc.chat) {
			t.Errorf("%v: got %+v", tc.missed, got[0])
		}
	}
}
