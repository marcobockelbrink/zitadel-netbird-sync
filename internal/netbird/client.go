// Package netbird is a small client for the parts of the NetBird management
// API the sync needs: listing and creating groups, listing users and setting a
// user's groups.
package netbird

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxResponseBytes = 32 << 20

// Group is a NetBird group.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// User is a NetBird user. Role and IsBlocked are carried only because the
// update call requires them; the sync passes them back unchanged.
type User struct {
	ID            string   `json:"id"`
	Email         string   `json:"email"`
	Role          string   `json:"role"`
	AutoGroups    []string `json:"auto_groups"`
	IsBlocked     bool     `json:"is_blocked"`
	IsServiceUser bool     `json:"is_service_user"`
}

// Client talks to the NetBird management API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the given management URL.
func New(baseURL, token string, hc *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: hc}
}

// Groups lists all groups of the account.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var out []Group
	return out, c.do(ctx, http.MethodGet, "/api/groups", nil, &out)
}

// CreateGroup creates an empty group.
func (c *Client) CreateGroup(ctx context.Context, name string) (Group, error) {
	var out Group
	err := c.do(ctx, http.MethodPost, "/api/groups", map[string]any{"name": name}, &out)
	if err == nil && out.ID == "" {
		err = fmt.Errorf("netbird: created group %q but got no id back", name)
	}
	return out, err
}

// Users lists all users of the account, service users included.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out []User
	return out, c.do(ctx, http.MethodGet, "/api/users", nil, &out)
}

// SetUserGroups replaces the user's group list. Role and blocked state are
// sent back exactly as they were read.
func (c *Client) SetUserGroups(ctx context.Context, u User, groupIDs []string) error {
	if groupIDs == nil {
		groupIDs = []string{}
	}
	body := map[string]any{"role": u.Role, "is_blocked": u.IsBlocked, "auto_groups": groupIDs}
	return c.do(ctx, http.MethodPut, "/api/users/"+url.PathEscape(u.ID), body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("netbird %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body may hold account data; the status is enough to act on.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return fmt.Errorf("netbird %s %s: unexpected status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("netbird %s %s: decode response: %w", method, path, err)
	}
	return nil
}
