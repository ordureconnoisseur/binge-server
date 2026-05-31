package redgifs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Client wraps Redgifs' temp-token auth + gif lookup endpoint.
// One temp token lasts ~24h; we hold it in-process and refresh on 401.
type Client struct {
	userAgent string
	http      *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

func New(userAgent string) *Client {
	return &Client{
		userAgent: userAgent,
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

var ErrNotFound = errors.New("redgifs gif not found")

type Resolved struct {
	HD  string
	SD  string
	GIF string
}

func (c *Client) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.exp) > 5*time.Minute {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.redgifs.com/v2/auth/temporary", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("redgifs token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MB cap
	if resp.StatusCode != 200 {
		return fmt.Errorf("redgifs token %d: %s", resp.StatusCode, raw)
	}
	var tok struct {
		Token   string `json:"token"`
		Addr    string `json:"addr"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("redgifs token decode: %w", err)
	}
	if tok.Token == "" {
		return fmt.Errorf("redgifs token empty (body=%s)", raw)
	}
	c.token = tok.Token
	// Token tells us its own expiry via JWT but parsing JWT here is
	// overkill — temp tokens are documented as 24h. Refresh slightly
	// earlier for safety.
	c.exp = time.Now().Add(23 * time.Hour)
	return nil
}

// Resolve looks up a gif by its slug and returns direct media URLs.
// HD is preferred; SD is provided as a fallback if HD is empty.
func (c *Client) Resolve(ctx context.Context, slug string) (Resolved, error) {
	if slug == "" {
		return Resolved{}, fmt.Errorf("empty slug")
	}
	if err := c.ensureToken(ctx); err != nil {
		return Resolved{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.redgifs.com/v2/gifs/"+slug, nil)
	if err != nil {
		return Resolved{}, err
	}
	c.mu.Lock()
	req.Header.Set("Authorization", "Bearer "+c.token)
	c.mu.Unlock()
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return Resolved{}, fmt.Errorf("redgifs gif request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 401:
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		return Resolved{}, fmt.Errorf("redgifs 401 — token invalidated, will retry next poll")
	case 404, 410:
		return Resolved{}, ErrNotFound
	default:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MB cap
		return Resolved{}, fmt.Errorf("redgifs %d: %s", resp.StatusCode, raw)
	}

	var body struct {
		Gif struct {
			URLs struct {
				HD  string `json:"hd"`
				SD  string `json:"sd"`
				GIF string `json:"gif"`
			} `json:"urls"`
		} `json:"gif"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Resolved{}, fmt.Errorf("redgifs gif decode: %w", err)
	}
	return Resolved{
		HD:  body.Gif.URLs.HD,
		SD:  body.Gif.URLs.SD,
		GIF: body.Gif.URLs.GIF,
	}, nil
}
