// Package zitadel reads projects, users and user grants from a Zitadel
// instance. It only ever reads.
package zitadel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// pageSize stays well below Zitadel's default query limit, so a server-side
// cap can never silently shorten a page.
const pageSize = 100

const maxResponseBytes = 32 << 20

// Project is an active Zitadel project.
type Project struct {
	ID   string
	Name string
}

// Grant says that a user holds at least one role on a project.
type Grant struct {
	UserID    string
	ProjectID string
}

// Client talks to the Zitadel management API.
type Client struct {
	baseURL string
	token   string
	orgID   string
	http    *http.Client
}

// New returns a client. orgID may be empty; the token's own organization is
// used then.
func New(baseURL, token, orgID string, hc *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, orgID: orgID, http: hc}
}

// ActiveProjects lists the projects owned by the organization.
func (c *Client) ActiveProjects(ctx context.Context) ([]Project, error) {
	var out []Project
	err := c.search(ctx, "/management/v1/projects/_search", func(raw json.RawMessage) error {
		var p struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if p.State == "PROJECT_STATE_ACTIVE" && p.ID != "" && p.Name != "" {
			out = append(out, Project{ID: p.ID, Name: p.Name})
		}
		return nil
	})
	return out, err
}

// ActiveGrants lists the active user grants of the organization.
func (c *Client) ActiveGrants(ctx context.Context) ([]Grant, error) {
	var out []Grant
	err := c.search(ctx, "/management/v1/users/grants/_search", func(raw json.RawMessage) error {
		var g struct {
			UserID    string `json:"userId"`
			ProjectID string `json:"projectId"`
			State     string `json:"state"`
		}
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		if g.State == "USER_GRANT_STATE_ACTIVE" && g.UserID != "" && g.ProjectID != "" {
			out = append(out, Grant{UserID: g.UserID, ProjectID: g.ProjectID})
		}
		return nil
	})
	return out, err
}

// ActiveUserEmails maps user ID to email address for every active human user
// whose address is verified. An unverified address is ignored: it is a claim,
// not proof, and group membership must not follow a claim.
func (c *Client) ActiveUserEmails(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	err := c.search(ctx, "/management/v1/users/_search", func(raw json.RawMessage) error {
		var u struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Human *struct {
				Email struct {
					Email    string `json:"email"`
					Verified bool   `json:"isEmailVerified"`
				} `json:"email"`
			} `json:"human"`
		}
		if err := json.Unmarshal(raw, &u); err != nil {
			return err
		}
		if u.State != "USER_STATE_ACTIVE" || u.Human == nil {
			return nil
		}
		if e := strings.TrimSpace(u.Human.Email.Email); e != "" && u.Human.Email.Verified {
			out[u.ID] = e
		}
		return nil
	})
	return out, err
}

// search pages through a Zitadel list endpoint. It trusts the reported total
// rather than the page length, and fails instead of returning a partial list:
// a short list would look like revoked grants to the caller.
func (c *Client) search(ctx context.Context, path string, each func(json.RawMessage) error) error {
	offset := 0
	for {
		body := map[string]any{"query": map[string]any{"offset": offset, "limit": pageSize, "asc": true}}
		var page struct {
			Details struct {
				TotalResult json.RawMessage `json:"totalResult"`
			} `json:"details"`
			Result []json.RawMessage `json:"result"`
		}
		if err := c.post(ctx, path, body, &page); err != nil {
			return err
		}
		for _, r := range page.Result {
			if err := each(r); err != nil {
				return fmt.Errorf("zitadel %s: decode entry: %w", path, err)
			}
		}
		offset += len(page.Result)
		total, err := parseTotal(page.Details.TotalResult)
		if err != nil {
			return fmt.Errorf("zitadel %s: %w", path, err)
		}
		if offset >= total {
			return nil
		}
		if len(page.Result) == 0 {
			return fmt.Errorf("zitadel %s: got %d of %d entries, then an empty page", path, offset, total)
		}
	}
}

// parseTotal reads Zitadel's totalResult, which is a uint64 rendered as a JSON
// string and omitted when zero.
func parseTotal(raw json.RawMessage) (int, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, errors.New("unreadable totalResult")
	}
	return n, nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.orgID != "" {
		req.Header.Set("x-zitadel-orgid", c.orgID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zitadel %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body may hold account data; the status is enough to act on.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return fmt.Errorf("zitadel %s: unexpected status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("zitadel %s: decode response: %w", path, err)
	}
	return nil
}
