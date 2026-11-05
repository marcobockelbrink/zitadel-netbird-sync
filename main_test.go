package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Neither API redirects. Following one could carry the token to another
// address or over plain http, so the client must not follow redirects.
func TestHTTPClientDoesNotFollowRedirects(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the redirect itself (307)", resp.StatusCode)
	}
	if leaked != "" {
		t.Errorf("redirect was followed, target saw Authorization %q", leaked)
	}
}
