package markup

import "testing"

func TestPango(t *testing.T) {
	name := func(j string) string { return map[string]string{"919800000001@s.whatsapp.net": "Asha <3"}[j] }
	cases := []struct{ in, want string }{
		{"a < b & c", "a &lt; b &amp; c"},
		{"*bold* and _it_ and ~gone~", "<b>bold</b> and <i>it</i> and <s>gone</s>"},
		{"*a* *b*", "<b>a</b> <b>b</b>"},
		{"2*3*4 stays", "2*3*4 stays"},
		{"```x*y*```", "<tt>x*y*</tt>"},
		{"see https://ex.com/a_b_c.", `see <a href="https://ex.com/a_b_c">https://ex.com/a_b_c</a>.`},
		{"www.ex.com", `<a href="https://www.ex.com">www.ex.com</a>`},
		{"hi @919800000001!", "hi <b>@Asha &lt;3</b>!"},
		{"call @919800000002", "call @919800000002"}, // typed, not tagged
	}
	for _, c := range cases {
		if got := Pango(c.in, []string{"919800000001@s.whatsapp.net"}, name); got != c.want {
			t.Errorf("%q:\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
