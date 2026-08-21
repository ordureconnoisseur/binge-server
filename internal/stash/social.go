package stash

import (
	"context"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Methods backing the binge-server "save a social post into Stash"
// pipeline: scan a placed file, find its new Stash entity by path,
// ensure source Studio/Tag exist (by NAME — never hardcoded ids, so this
// works in any user's Stash), and write the metadata.

// MetadataScan kicks off a scan of the given paths. Async on Stash's
// side; the caller polls FindSceneIDByPath / FindImageIDByPath until the
// entity appears.
func (c *Client) MetadataScan(ctx context.Context, paths []string) error {
	const q = `mutation($p:[String!]){ metadataScan(input:{paths:$p, rescan:false}) }`
	return c.do(ctx, q, map[string]any{"p": paths}, nil)
}

// normPath lowercases + forward-slashes a path for tolerant comparison
// (Stash stores Windows backslash paths; callers build either form).
func normPath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
}

// FindSceneIDByPath returns the id of the scene whose file path matches
// exactPath. `token` is a distinctive substring (e.g. the unique post id
// in the filename) used to narrow the server-side query; the exact match
// is then verified client-side, since Stash's path filter is word-based.
func (c *Client) FindSceneIDByPath(ctx context.Context, token, exactPath string) (string, error) {
	const q = `query($t:String!){ findScenes(scene_filter:{path:{value:$t,modifier:INCLUDES}}, filter:{per_page:100}){ scenes{ id files{ path } } } }`
	var resp struct {
		FindScenes struct {
			Scenes []struct {
				ID    string `json:"id"`
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			} `json:"scenes"`
		} `json:"findScenes"`
	}
	if err := c.do(ctx, q, map[string]any{"t": token}, &resp); err != nil {
		return "", err
	}
	want := normPath(exactPath)
	for _, s := range resp.FindScenes.Scenes {
		for _, f := range s.Files {
			if normPath(f.Path) == want {
				return s.ID, nil
			}
		}
	}
	return "", nil // not found yet (scan may still be running)
}

// FindImageIDByPath is FindSceneIDByPath for images.
func (c *Client) FindImageIDByPath(ctx context.Context, token, exactPath string) (string, error) {
	const q = `query($t:String!){ findImages(image_filter:{path:{value:$t,modifier:INCLUDES}}, filter:{per_page:100}){ images{ id visual_files{ ... on ImageFile { path } ... on VideoFile { path } } } } }`
	var resp struct {
		FindImages struct {
			Images []struct {
				ID          string `json:"id"`
				VisualFiles []struct {
					Path string `json:"path"`
				} `json:"visual_files"`
			} `json:"images"`
		} `json:"findImages"`
	}
	if err := c.do(ctx, q, map[string]any{"t": token}, &resp); err != nil {
		return "", err
	}
	want := normPath(exactPath)
	for _, im := range resp.FindImages.Images {
		for _, f := range im.VisualFiles {
			if normPath(f.Path) == want {
				return im.ID, nil
			}
		}
	}
	return "", nil
}

// EnsureStudio returns the id of the studio named `name`, creating it if
// absent. Resolution is by name so a shipped install auto-provisions its
// own "X" / "Reddit" / etc. studios.
func (c *Client) EnsureStudio(ctx context.Context, name string) (string, error) {
	var find struct {
		FindStudios struct {
			Studios []struct {
				ID string `json:"id"`
			} `json:"studios"`
		} `json:"findStudios"`
	}
	const fq = `query($n:String!){ findStudios(studio_filter:{name:{value:$n,modifier:EQUALS}}){ studios{ id } } }`
	if err := c.do(ctx, fq, map[string]any{"n": name}, &find); err != nil {
		return "", err
	}
	if len(find.FindStudios.Studios) > 0 {
		return find.FindStudios.Studios[0].ID, nil
	}
	var create struct {
		StudioCreate struct {
			ID string `json:"id"`
		} `json:"studioCreate"`
	}
	const cq = `mutation($n:String!){ studioCreate(input:{name:$n}){ id } }`
	if err := c.do(ctx, cq, map[string]any{"n": name}, &create); err != nil {
		return "", err
	}
	return create.StudioCreate.ID, nil
}

// EnsureTag returns the id of tag `name`, creating it (optionally under
// parent `parentName`, which is itself ensured) if absent.
func (c *Client) EnsureTag(ctx context.Context, name, parentName string) (string, error) {
	findTag := func(n string) (string, error) {
		var find struct {
			FindTags struct {
				Tags []struct {
					ID string `json:"id"`
				} `json:"tags"`
			} `json:"findTags"`
		}
		const fq = `query($n:String!){ findTags(tag_filter:{name:{value:$n,modifier:EQUALS}}){ tags{ id } } }`
		if err := c.do(ctx, fq, map[string]any{"n": n}, &find); err != nil {
			return "", err
		}
		if len(find.FindTags.Tags) > 0 {
			return find.FindTags.Tags[0].ID, nil
		}
		return "", nil
	}
	if id, err := findTag(name); err != nil || id != "" {
		return id, err
	}
	vars := map[string]any{"n": name}
	q := `mutation($n:String!){ tagCreate(input:{name:$n}){ id } }`
	if parentName != "" {
		parentID, err := c.EnsureTag(ctx, parentName, "")
		if err != nil {
			return "", err
		}
		vars["p"] = []string{parentID}
		q = `mutation($n:String!,$p:[ID!]){ tagCreate(input:{name:$n, parent_ids:$p}){ id } }`
	}
	var create struct {
		TagCreate struct {
			ID string `json:"id"`
		} `json:"tagCreate"`
	}
	if err := c.do(ctx, q, vars, &create); err != nil {
		return "", err
	}
	return create.TagCreate.ID, nil
}

// EntityMeta is the metadata applied to a freshly-saved scene/image.
type EntityMeta struct {
	PerformerID string // optional ("" = skip)
	StudioID    string
	TagID       string
	URL         string
	Date        string // "YYYY-MM-DD"
	Details     string
}

func (m EntityMeta) vars(id string) map[string]any {
	v := map[string]any{"id": id, "studio": m.StudioID, "tags": []string{m.TagID}}
	if m.PerformerID != "" {
		v["perfs"] = []string{m.PerformerID}
	} else {
		v["perfs"] = []string{}
	}
	if m.URL != "" {
		v["urls"] = []string{m.URL}
	} else {
		v["urls"] = []string{}
	}
	v["date"] = m.Date
	v["details"] = m.Details
	return v
}

// UpdateSceneMeta writes the saved-post metadata onto a scene.
func (c *Client) UpdateSceneMeta(ctx context.Context, id string, m EntityMeta) error {
	const q = `mutation($id:ID!,$perfs:[ID!],$studio:ID,$tags:[ID!],$urls:[String!],$date:String,$details:String){
		sceneUpdate(input:{id:$id, performer_ids:$perfs, studio_id:$studio, tag_ids:$tags, urls:$urls, date:$date, details:$details}){ id }
	}`
	return c.do(ctx, q, m.vars(id), nil)
}

// UpdateImageMeta writes the saved-post metadata onto an image.
func (c *Client) UpdateImageMeta(ctx context.Context, id string, m EntityMeta) error {
	const q = `mutation($id:ID!,$perfs:[ID!],$studio:ID,$tags:[ID!],$urls:[String!],$date:String,$details:String){
		imageUpdate(input:{id:$id, performer_ids:$perfs, studio_id:$studio, tag_ids:$tags, urls:$urls, date:$date, details:$details}){ id }
	}`
	return c.do(ctx, q, m.vars(id), nil)
}

// PerformerHandle pulls a handle for `source` from a performer's urls[]
// (e.g. instagram.com/<h>, x.com/<h>, reddit.com/u/<h>, redgifs.com/users/<h>).
// Empty if none — caller falls back to the performer name.
// Path segments that belong to the site rather than to a person.
var reservedSegment = map[string]bool{
	// x / twitter
	"i": true, "home": true, "search": true, "explore": true,
	"notifications": true, "messages": true, "intent": true,
	"share": true, "hashtag": true, "settings": true, "compose": true,
	"login": true, "signup": true, "about": true, "tos": true,
	"privacy": true, "status": true,
	// instagram
	"p": true, "reel": true, "reels": true, "stories": true,
	"accounts": true, "direct": true, "tv": true,
	// generic
	"": true, ".": true, "..": true,
}

func PerformerHandle(p Performer, source string) string {
	// host suffix -> the path prefixes that introduce a handle on it.
	//
	// Matched against the parsed host, not by searching the whole URL
	// for a substring. Searching found "reddit.com/user/" inside
	// https://elsewhere.example/reddit.com/user/victim and handed back
	// victim, and this value picks the folder a save is written into.
	// The poller was fixed to parse the host; this copy was not, and it
	// is the one the save path uses.
	type rule struct {
		hosts    []string
		prefixes []string
	}
	rules := map[string]rule{
		"x": {
			hosts:    []string{"x.com", "twitter.com"},
			prefixes: []string{"/"},
		},
		"instagram": {
			hosts:    []string{"instagram.com"},
			prefixes: []string{"/"},
		},
		"reddit": {
			hosts:    []string{"reddit.com"},
			prefixes: []string{"/u/", "/user/", "/r/"},
		},
		"redgifs": {
			hosts:    []string{"redgifs.com"},
			prefixes: []string{"/users/", "/u/"},
		},
	}
	r, ok := rules[source]
	if !ok {
		return ""
	}
	for _, raw := range p.URLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u == nil {
			continue
		}
		// A performer's urls[] is typed by hand, so a bare
		// "reddit.com/u/x" with no scheme is ordinary; url.Parse reads
		// that as a path with no host.
		if u.Host == "" && !strings.HasPrefix(raw, "/") {
			if u2, err2 := url.Parse("https://" + raw); err2 == nil {
				u = u2
			}
		}
		host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
		matched := false
		for _, h := range r.hosts {
			if host == h || strings.HasSuffix(host, "."+h) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// Unescaped, so a non-ASCII handle names its folder as itself
		// rather than as percent-escapes.
		path := u.Path
		for _, prefix := range r.prefixes {
			if !strings.HasPrefix(strings.ToLower(path), prefix) {
				continue
			}
			rest := strings.TrimRight(path[len(prefix):], "/")
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				rest = rest[:j]
			}
			// A site's own pages are not people. x.com/i/status/... and
			// instagram.com/p/... gave "i" and "p", and this value names
			// the folder a save is written into, so a library ended up
			// with directories called after the site's routing. The
			// poller already skips these for x; the two copies were
			// harmonised on host parsing and not on this.
			if rest == "" || reservedSegment[strings.ToLower(rest)] {
				continue
			}
			return rest
		}
	}
	return ""
}

// OwnedPornhubViewkeys returns the set of PornHub viewkeys for scenes
// already in the library — any scene whose url contains "pornhub.com".
// Used to dedup the PornHub feed/stories so videos the user already has
// (saved through binge OR imported some other way) don't resurface.
// Paginated; the viewkey is read from each url's `viewkey` query param.
func (c *Client) OwnedPornhubViewkeys(ctx context.Context) (map[string]struct{}, error) {
	const q = `
query BingeOwnedPornhub($page: Int!, $perPage: Int!) {
  findScenes(
    filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }
    scene_filter: { url: { value: "pornhub.com", modifier: INCLUDES } }
  ) {
    count
    scenes { urls }
  }
}`
	const perPage = 500
	out := map[string]struct{}{}
	for page := 1; ; page++ {
		var resp struct {
			FindScenes struct {
				Count  int `json:"count"`
				Scenes []struct {
					URLs []string `json:"urls"`
				} `json:"scenes"`
			} `json:"findScenes"`
		}
		if err := c.do(ctx, q, map[string]any{"page": page, "perPage": perPage}, &resp); err != nil {
			return nil, err
		}
		for _, sc := range resp.FindScenes.Scenes {
			for _, u := range sc.URLs {
				if vk := PornhubViewkey(u); vk != "" {
					out[vk] = struct{}{}
				}
			}
		}
		if len(resp.FindScenes.Scenes) == 0 || page*perPage >= resp.FindScenes.Count {
			break
		}
	}
	return out, nil
}

// PornhubViewkey extracts the `viewkey` from a pornhub.com watch URL,
// or "" if the url isn't a pornhub link / has no viewkey. The viewkey is
// PornHub's stable per-video id (matches the daemon's pornhub_videos
// primary key) so this is an exact, not fuzzy, dedup key.
func PornhubViewkey(raw string) string {
	if !strings.Contains(strings.ToLower(raw), "pornhub.com") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Query().Get("viewkey")
}

// SanitizeSegment makes a string safe to use as a single path segment
// (folder/file name) — strips path separators and trims.
// Longest segment we will write. Both limits are real and neither
// implies the other: ext4 measures a name in bytes and stops at 255,
// NTFS measures it in UTF-16 units and stops at 255, and a character
// can be four bytes in one and two units in the other. Eighty
// characters keeps names readable; 200 bytes keeps the widest of them
// inside the byte limit with room for the extension.
const (
	maxSegmentRunes = 80
	maxSegmentBytes = 200
)

// truncateSegment cuts to the first limit reached, always on a character
// boundary.
func truncateSegment(s string) string {
	if len(s) <= maxSegmentBytes && utf8.RuneCountInString(s) <= maxSegmentRunes {
		return s
	}
	runes := 0
	for i := range s {
		// range over a string yields byte offsets at character starts,
		// so i is always a safe place to cut.
		if runes >= maxSegmentRunes || i > maxSegmentBytes {
			return s[:i]
		}
		runes++
	}
	return s
}

func SanitizeSegment(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		case '%':
			// yt-dlp reads its -o argument as an output TEMPLATE, so a
			// %(title)s in a caller-supplied id made it write to a name
			// derived from the video rather than the one we asked for -
			// the rename then failed, the request 502'd, and the
			// cleanup removed the name we asked for rather than the one
			// it used, leaving an unreferenced file in the library. A
			// bare % is worse: it fails the template outright, so every
			// save for a performer named "100% ..." errored.
			return '_'
		}
		// Control characters are not legal in a Windows filename and
		// stop the write dead with "invalid argument", which surfaces
		// to the user as a failed save with nothing explaining why.
		// They arrive here from performer names and from a
		// caller-supplied handle, so they are worth cleaning rather
		// than refusing.
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, s)
	s = strings.TrimSpace(strings.Trim(s, "._"))
	if s == "" {
		return "unknown"
	}
	// Truncate on a character boundary.
	//
	// This used to slice bytes, which cuts a multi-byte character in
	// half: a CJK handle came back as invalid UTF-8, and the two sides
	// then disagreed about the name. Go re-encodes the broken tail as
	// U+FFFD on the way to the Windows syscall, so the file landed under
	// a name that was NOT the string we were holding - and since that
	// string is also what we hand Stash as the scan path and later look
	// the scene up by, the tag pass could never match it. The media
	// saved and stayed untagged forever, retrying for five minutes each
	// time.
	//
	// Slicing runes also stops two different handles that share a long
	// prefix from truncating onto each other and pooling two performers'
	// media into one folder.
	if cut := truncateSegment(s); cut != s {
		// The cut can expose a dot or space that Windows silently
		// strips from the stored name, which would desync the two
		// sides again.
		s = strings.TrimSpace(strings.Trim(cut, "._"))
		if s == "" {
			return "unknown"
		}
	}
	return s
}
