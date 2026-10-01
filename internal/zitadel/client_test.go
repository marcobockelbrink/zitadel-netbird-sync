package zitadel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pagedServer serves `total` projects, `perPage` at most per request, and
// reports `claimed` as the total.
func pagedServer(t *testing.T, total, perPage, claimed int, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("x-zitadel-orgid"); got != "org1" {
			t.Errorf("x-zitadel-orgid = %q", got)
		}
		var req struct {
			Query struct {
				Offset int `json:"offset"`
				Limit  int `json:"limit"`
			} `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		n := min(req.Query.Limit, perPage)
		var items []string
		for i := req.Query.Offset; i < total && len(items) < n; i++ {
			state := "PROJECT_STATE_ACTIVE"
			if i == 0 {
				state = "PROJECT_STATE_INACTIVE"
			}
			items = append(items, fmt.Sprintf(`{"id":"%d","name":"p%d","state":"%s"}`, i, i, state))
		}
		fmt.Fprintf(w, `{"details":{"totalResult":"%d"},"result":[%s]}`, claimed, strings.Join(items, ","))
	}))
}

func TestActiveProjectsFollowsPages(t *testing.T) {
	calls := 0
	srv := pagedServer(t, 250, 100, 250, &calls)
	defer srv.Close()

	got, err := New(srv.URL, "secret", "org1", srv.Client()).ActiveProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 249 {
		t.Errorf("got %d active projects, want 249 (one is inactive)", len(got))
	}
	if calls != 3 {
		t.Errorf("made %d requests, want 3", calls)
	}
}

// A server that caps pages below the requested size must not shorten the list.
func TestSearchSurvivesServerSideCap(t *testing.T) {
	calls := 0
	srv := pagedServer(t, 30, 10, 30, &calls)
	defer srv.Close()

	got, err := New(srv.URL, "secret", "org1", srv.Client()).ActiveProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 29 {
		t.Errorf("got %d, want 29", len(got))
	}
}

func TestSearchFailsOnIncompleteList(t *testing.T) {
	calls := 0
	srv := pagedServer(t, 5, 100, 9, &calls)
	defer srv.Close()

	if _, err := New(srv.URL, "secret", "org1", srv.Client()).ActiveProjects(context.Background()); err == nil {
		t.Fatal("want error when fewer entries arrive than the server reports")
	}
}

func TestErrorDoesNotLeakBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"private detail"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "secret", "", srv.Client()).ActiveGrants(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "private detail") || strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks response body or token: %v", err)
	}
}

func TestActiveUserEmailsOnlyVerifiedActiveHumans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"details":{"totalResult":"4"},"result":[
			{"id":"1","state":"USER_STATE_ACTIVE","human":{"email":{"email":"a@example.org","isEmailVerified":true}}},
			{"id":"2","state":"USER_STATE_ACTIVE","human":{"email":{"email":"b@example.org"}}},
			{"id":"3","state":"USER_STATE_INACTIVE","human":{"email":{"email":"c@example.org","isEmailVerified":true}}},
			{"id":"4","state":"USER_STATE_ACTIVE","machine":{"name":"robot"}}
		]}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "secret", "", srv.Client()).ActiveUserEmails(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["1"] != "a@example.org" {
		t.Errorf("got %v, want only user 1", got)
	}
}

func TestActiveGrantsEmptyOrganization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"details":{}}`)
	}))
	defer srv.Close()

	got, err := New(srv.URL, "secret", "", srv.Client()).ActiveGrants(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want empty list without error", got, err)
	}
}
