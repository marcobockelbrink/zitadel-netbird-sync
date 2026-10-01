package netbird

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetUserGroupsSendsRoleAndBlockedBack(t *testing.T) {
	var method, path, auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	u := User{ID: "google|a/b", Role: "admin", IsBlocked: true}
	if err := New(srv.URL, "tok", srv.Client()).SetUserGroups(context.Background(), u, nil); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || path != "/api/users/google%7Ca%2Fb" {
		t.Errorf("request = %s %s", method, path)
	}
	if auth != "Token tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if body["role"] != "admin" || body["is_blocked"] != true {
		t.Errorf("role/is_blocked not passed back unchanged: %v", body)
	}
	// An empty list must be [] and not null, or the API rejects the update.
	if groups, ok := body["auto_groups"].([]any); !ok || len(groups) != 0 {
		t.Errorf("auto_groups = %#v, want empty list", body["auto_groups"])
	}
}

func TestListsDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/groups":
			fmt.Fprint(w, `[{"id":"g1","name":"idp-alpha","peers_count":3}]`)
		case "/api/users":
			fmt.Fprint(w, `[{"id":"u1","email":"a@example.org","role":"user","auto_groups":["g1"],"is_service_user":false}]`)
		}
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "tok", srv.Client())
	groups, err := c.Groups(context.Background())
	if err != nil || len(groups) != 1 || groups[0].Name != "idp-alpha" {
		t.Errorf("groups = %v, %v", groups, err)
	}
	users, err := c.Users(context.Background())
	if err != nil || len(users) != 1 || users[0].AutoGroups[0] != "g1" {
		t.Errorf("users = %v, %v", users, err)
	}
}

func TestCreateGroup(t *testing.T) {
	answer := `{"id":"g9","name":"idp-beta"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/groups" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, answer)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", srv.Client())
	g, err := c.CreateGroup(context.Background(), "idp-beta")
	if err != nil || g.ID != "g9" {
		t.Errorf("got %v, %v", g, err)
	}

	answer = `{}`
	if _, err := c.CreateGroup(context.Background(), "idp-beta"); err == nil {
		t.Error("want error when the answer carries no id")
	}
}

func TestErrorDoesNotLeakBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"private detail"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok", srv.Client()).Users(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "private detail") || strings.Contains(err.Error(), "tok") {
		t.Errorf("error leaks response body or token: %v", err)
	}
}
