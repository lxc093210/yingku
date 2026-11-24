// Package alist is a minimal client for the Alist v3 HTTP API
// (batch 29). It only ever lists directories and resolves raw
// playback URLs — it never opens, downloads or probes the files
// themselves (the user's 115 anti-risk-control red line).
package alist

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PoliteDelay is the minimum spacing between two API calls: remote
// scans list thousands of folders and must not hammer the server.
const PoliteDelay = 150 * time.Millisecond

// Entry is one row of an /api/fs/list response.
type Entry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// Client talks to one Alist server with one account. The auth token
// is cached in memory and refreshed lazily: the first call logs in,
// and a 401 triggers exactly one re-login + retry.
type Client struct {
	base     string
	username string
	password string

	http *http.Client

	mu       sync.Mutex
	token    string
	lastCall time.Time
}

// New builds a client. base is the server root, e.g.
// http://192.168.1.5:5678 (trailing slash trimmed).
func New(base, username, password string) *Client {
	return &Client{
		base:     strings.TrimRight(strings.TrimSpace(base), "/"),
		username: strings.TrimSpace(username),
		password: password,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Configured reports whether the client has a base URL at all.
func (c *Client) Configured() bool { return c.base != "" }

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// call performs one API request with the politeness delay and the
// lazy-login / single-relogin-on-401 policy. path is the API path
// ("/api/fs/list"); body is JSON-marshalled; out receives data.
func (c *Client) call(path string, body any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		if err := c.loginLocked(); err != nil {
			return err
		}
	}
	code, err := c.doLocked(path, body, out)
	if err == nil {
		return nil
	}
	if code == 401 {
		// Token expired or was revoked: re-login once and retry.
		c.token = ""
		if lerr := c.loginLocked(); lerr != nil {
			return lerr
		}
		_, err = c.doLocked(path, body, out)
	}
	return err
}

// doLocked issues the request once. It returns the Alist envelope
// code alongside the error so callers can spot 401s.
func (c *Client) doLocked(path string, body any, out any) (int, error) {
	c.politeLocked()
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("alist %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, fmt.Errorf("alist %s: read body: %w", path, err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, fmt.Errorf("alist %s: HTTP %d, undecodable response", path, resp.StatusCode)
	}
	if env.Code != 200 {
		return env.Code, fmt.Errorf("alist %s: code %d: %s", path, env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return env.Code, fmt.Errorf("alist %s: decode data: %w", path, err)
		}
	}
	return env.Code, nil
}

// politeLocked sleeps until PoliteDelay has passed since the
// previous call. Caller holds mu.
func (c *Client) politeLocked() {
	if !c.lastCall.IsZero() {
		if wait := PoliteDelay - time.Since(c.lastCall); wait > 0 {
			time.Sleep(wait)
		}
	}
	c.lastCall = time.Now()
}

// loginLocked authenticates and caches the token. Caller holds mu.
func (c *Client) loginLocked() error {
	c.politeLocked()
	raw, _ := json.Marshal(map[string]string{
		"username": c.username,
		"password": c.password,
	})
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("alist login: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("alist login: read body: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("alist login: HTTP %d, undecodable response", resp.StatusCode)
	}
	if env.Code != 200 {
		return fmt.Errorf("alist login: code %d: %s", env.Code, env.Message)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(env.Data, &tok); err != nil || tok.Token == "" {
		return fmt.Errorf("alist login: no token in response")
	}
	c.token = tok.Token
	return nil
}

// List returns every entry of a remote directory, following the
// API's pagination until all rows are collected.
func (c *Client) List(path string) ([]Entry, error) {
	const perPage = 500
	var out []Entry
	for page := 1; ; page++ {
		var data struct {
			Total   int     `json:"total"`
			Content []Entry `json:"content"`
		}
		err := c.call("/api/fs/list", map[string]any{
			"path": path, "page": page, "per_page": perPage, "refresh": false,
		}, &data)
		if err != nil {
			return nil, err
		}
		out = append(out, data.Content...)
		if len(data.Content) == 0 || len(out) >= data.Total {
			return out, nil
		}
	}
}

// GetRawURL resolves the direct playback URL of one remote file.
func (c *Client) GetRawURL(path string) (string, error) {
	var data struct {
		RawURL string `json:"raw_url"`
	}
	if err := c.call("/api/fs/get", map[string]any{"path": path}, &data); err != nil {
		return "", err
	}
	if data.RawURL == "" {
		return "", fmt.Errorf("alist get %s: empty raw_url", path)
	}
	return data.RawURL, nil
}
