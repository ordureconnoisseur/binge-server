package reddit

import "testing"

// The settings page asks for the value of the reddit_session row, which
// is the right thing to ask for. Sent as-is it is not a cookie at all,
// and every Reddit request comes back 403.
func TestNormalizeCookie(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a bare value gets its name",
			in:   "eyJhbGciOiJIUzUxMiJ9.abc.def",
			want: "reddit_session=eyJhbGciOiJIUzUxMiJ9.abc.def",
		},
		{
			name: "surrounding whitespace from a copy is dropped",
			in:   "  eyJhbGciOiJIUzUxMiJ9.abc.def\n",
			want: "reddit_session=eyJhbGciOiJIUzUxMiJ9.abc.def",
		},
		{
			name: "an already-named cookie is left alone",
			in:   "reddit_session=eyJhbGciOiJIUzUxMiJ9.abc.def",
			want: "reddit_session=eyJhbGciOiJIUzUxMiJ9.abc.def",
		},
		{
			name: "a whole copied header is left alone",
			in:   "reddit_session=abc; csv=1; edgebucket=xyz",
			want: "reddit_session=abc; csv=1; edgebucket=xyz",
		},
		{
			name: "a value carrying an equals sign is still a value",
			in:   "abc123==",
			want: "reddit_session=abc123==",
		},
		{
			name: "a jar with no session is left for the probe to refuse",
			in:   "csv=1; edgebucket=xyz",
			want: "csv=1; edgebucket=xyz",
		},
		{
			name: "a stray leading separator is dropped",
			in:   "; reddit_session=abc",
			want: "reddit_session=abc",
		},
		{
			name: "the Name column copied by mistake is not made to look real",
			in:   "reddit_session",
			want: "reddit_session",
		},
		{
			name: "empty stays empty, so it still reads as not set up",
			in:   "",
			want: "",
		},
		{
			name: "whitespace only is not a cookie either",
			in:   "   ",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeCookie(c.in); got != c.want {
				t.Fatalf("NormalizeCookie(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Whichever way the cookie arrives, the client has to send a usable
// header. Both entry points normalise, because a value set at startup
// and a value rotated from the settings page take different routes in.
func TestClientNormalizesOnBothPaths(t *testing.T) {
	c := New("bare-value", "test/1")
	if got := c.currentCookie(); got != "reddit_session=bare-value" {
		t.Fatalf("New kept %q", got)
	}
	c.SetCookie("another-bare-value")
	if got := c.currentCookie(); got != "reddit_session=another-bare-value" {
		t.Fatalf("SetCookie kept %q", got)
	}
}
