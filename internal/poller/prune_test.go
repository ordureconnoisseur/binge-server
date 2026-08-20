package poller

import "testing"

// A sync that returns nothing is a failed sync, not an instruction to
// empty the library.
//
// The keep-set is built only from what Stash returned, and a Stash that
// answers 200 with an empty performer list is not an error to anything
// upstream. Reconciling against that deleted every performer, and the
// foreign key took every post with it: ninety days of Reddit history
// that cannot be re-fetched, because Reddit serves only the last 25
// submissions per handle.
func TestSafeToPrune(t *testing.T) {
	cases := []struct {
		name                     string
		keep, existing, removing int
		want                     bool
	}{
		{"a fresh database has nothing to protect", 0, 0, 0, true},
		{"stash returned nobody at all", 0, 52, 52, false},
		{"stash returned nobody, one row held", 0, 1, 1, false},
		{"an ordinary edit removing two of fifty", 48, 50, 2, true},
		{"removing exactly half is still too much", 25, 50, 25, false},
		{"removing just under half is allowed", 26, 50, 24, true},
		{"removing nearly everything is refused", 1, 50, 49, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := safeToPrune(c.keep, c.existing, c.removing)
			if got != c.want {
				t.Fatalf("safeToPrune(keep=%d, existing=%d, removing=%d) = %v (%q), want %v",
					c.keep, c.existing, c.removing, got, why, c.want)
			}
			if !got && why == "" {
				t.Fatal("refused without saying why")
			}
		})
	}
}
