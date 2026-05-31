package stash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	mu      sync.RWMutex
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// SetCredentials swaps the Stash URL + API key at runtime. The poller
// calls this at the top of every tick after reading the current values
// from the config store.
func (c *Client) SetCredentials(baseURL, apiKey string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.apiKey = apiKey
	c.mu.Unlock()
}

func (c *Client) current() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL, c.apiKey
}

type gqlError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	baseURL, apiKey := c.current()
	if baseURL == "" {
		return fmt.Errorf("stash base URL not configured")
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("ApiKey", apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20)) // 64 MB cap
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("stash graphql %d: %s", resp.StatusCode, raw)
	}
	var wrap struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors,omitempty"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("decode: %w (body=%s)", err, raw)
	}
	if len(wrap.Errors) > 0 {
		return fmt.Errorf("stash graphql errors: %+v", wrap.Errors)
	}
	if out != nil {
		if err := json.Unmarshal(wrap.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// Performer is the slim shape binge-server cares about — id, display
// info, plus the urls array we parse for reddit handles.
type Performer struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ImagePath string   `json:"image_path"`
	Favorite  bool     `json:"favorite"`
	URLs      []string `json:"urls"`
}

const performersQuery = `
query BingeServerAllPerformers($page: Int!, $perPage: Int!) {
  findPerformers(filter: { page: $page, per_page: $perPage, sort: "id", direction: ASC }) {
    count
    performers { id name image_path favorite urls }
  }
}`

func (c *Client) FetchPerformersPage(ctx context.Context, page, perPage int) ([]Performer, int, error) {
	var resp struct {
		FindPerformers struct {
			Count      int         `json:"count"`
			Performers []Performer `json:"performers"`
		} `json:"findPerformers"`
	}
	if err := c.do(ctx, performersQuery, map[string]any{"page": page, "perPage": perPage}, &resp); err != nil {
		return nil, 0, err
	}
	return resp.FindPerformers.Performers, resp.FindPerformers.Count, nil
}
