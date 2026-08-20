package poller

import (
	"context"
	"testing"

	"github.com/ordureconnoisseur/binge-server/internal/stash"
)

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

// Only handles marked ok are polled, and nothing else resets one, so a
// performer retired because their Reddit URL had a typo stayed retired
// after the user corrected it. Correcting the URL is the clearest
// possible statement that the old verdict no longer applies.
func TestCorrectingAHandleRevivesIt(t *testing.T) {
	p := testPoller(t)
	ctx := context.Background()
	addPerformer(t, p, 1, "typo", "notfound")
	addPerformer(t, p, 2, "fine", "unavailable")

	// Performer 1's URL is fixed in Stash; performer 2's is unchanged.
	err := p.upsertPerformersBatch(ctx, []stash.Performer{
		{ID: "1", Name: "one", URLs: []string{"https://reddit.com/user/corrected"}},
		{ID: "2", Name: "two", URLs: []string{"https://reddit.com/user/fine"}},
	}, map[int]bool{})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if got := statusOf(t, p, 1); got != "ok" {
		t.Fatalf("a corrected handle stayed %q", got)
	}
	if got := statusOf(t, p, 2); got != "unavailable" {
		t.Fatalf("an unchanged handle was revived anyway: %q", got)
	}
}
