package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noFile(string) ([]byte, error) { return nil, errors.New("no file") }

func valid() map[string]string {
	return map[string]string{
		"ZITADEL_URL":   "https://id.example.org/",
		"ZITADEL_TOKEN": "zt",
		"NETBIRD_TOKEN": "nt",
		"GROUP_PREFIX":  "idp-",
	}
}

func TestDefaultsAreSafe(t *testing.T) {
	c, err := Load(env(valid()), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if !c.DryRun {
		t.Error("DryRun must default to true")
	}
	if c.Interval != 10*time.Minute {
		t.Errorf("Interval = %s, want 10m", c.Interval)
	}
	if c.NetbirdURL != "https://api.netbird.io" || c.ZitadelURL != "https://id.example.org" {
		t.Errorf("urls = %q, %q", c.NetbirdURL, c.ZitadelURL)
	}
	if !c.LowercaseNames || c.RunOnce || c.MaxRemovals != 50 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	m := valid()
	m["DRY_RUN"] = "false"
	m["SYNC_INTERVAL"] = "90s"
	m["EXCLUDE_PROJECTS"] = " tools , ,infra"
	m["MAX_REMOVALS"] = "0"
	delete(m, "NETBIRD_TOKEN")
	m["NETBIRD_TOKEN_FILE"] = "/run/secrets/nb"
	read := func(p string) ([]byte, error) {
		if p != "/run/secrets/nb" {
			t.Errorf("read %q", p)
		}
		return []byte("from-file\n"), nil
	}
	c, err := Load(env(m), read)
	if err != nil {
		t.Fatal(err)
	}
	if c.DryRun || c.Interval != 90*time.Second || c.NetbirdToken != "from-file" || c.MaxRemovals != 0 {
		t.Errorf("overrides not applied: %+v", c)
	}
	if len(c.Exclude) != 2 || c.Exclude[1] != "infra" {
		t.Errorf("Exclude = %v", c.Exclude)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]func(map[string]string){
		"GROUP_PREFIX":  func(m map[string]string) { m["GROUP_PREFIX"] = "  " },
		"ZITADEL_URL":   func(m map[string]string) { m["ZITADEL_URL"] = "http://id.example.org" },
		"NETBIRD_URL":   func(m map[string]string) { m["NETBIRD_URL"] = "ftp://x" },
		"ZITADEL_TOKEN": func(m map[string]string) { delete(m, "ZITADEL_TOKEN") },
		"SYNC_INTERVAL": func(m map[string]string) { m["SYNC_INTERVAL"] = "5s" },
		"DRY_RUN":       func(m map[string]string) { m["DRY_RUN"] = "nope" },
		"MAX_REMOVALS":  func(m map[string]string) { m["MAX_REMOVALS"] = "-1" },
	}
	for name, mutate := range cases {
		m := valid()
		mutate(m)
		c, err := Load(env(m), noFile)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want an error naming it", name, err)
		}
		if name == "DRY_RUN" && !c.DryRun {
			t.Error("an unreadable DRY_RUN must not switch writing on")
		}
	}
}

func TestLoopbackHTTPAllowed(t *testing.T) {
	m := valid()
	m["ZITADEL_URL"] = "http://127.0.0.1:8080"
	if _, err := Load(env(m), noFile); err != nil {
		t.Errorf("loopback http rejected: %v", err)
	}
}
