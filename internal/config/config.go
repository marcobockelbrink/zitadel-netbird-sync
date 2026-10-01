// Package config reads the runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the sync needs to run.
type Config struct {
	ZitadelURL   string
	ZitadelToken string
	ZitadelOrgID string

	NetbirdURL   string
	NetbirdToken string

	// GroupPrefix marks the NetBird groups this tool owns. It never touches a
	// group whose name does not start with it, so it must not be empty.
	GroupPrefix    string
	LowercaseNames bool
	Include        []string
	Exclude        []string

	DryRun      bool
	Interval    time.Duration
	RunOnce     bool
	MaxRemovals int
}

const (
	defaultNetbirdURL  = "https://api.netbird.io"
	defaultInterval    = 10 * time.Minute
	minInterval        = time.Minute
	defaultMaxRemovals = 50
)

// Load builds a Config. getenv and readFile are injected so tests need neither
// the process environment nor the file system.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var errs []error
	c := Config{
		ZitadelOrgID: strings.TrimSpace(getenv("ZITADEL_ORG_ID")),
		GroupPrefix:  getenv("GROUP_PREFIX"),
		Include:      splitList(getenv("INCLUDE_PROJECTS")),
		Exclude:      splitList(getenv("EXCLUDE_PROJECTS")),
	}

	var err error
	if c.ZitadelURL, err = baseURL("ZITADEL_URL", getenv("ZITADEL_URL"), ""); err != nil {
		errs = append(errs, err)
	}
	if c.NetbirdURL, err = baseURL("NETBIRD_URL", getenv("NETBIRD_URL"), defaultNetbirdURL); err != nil {
		errs = append(errs, err)
	}
	if c.ZitadelToken, err = secret("ZITADEL_TOKEN", getenv, readFile); err != nil {
		errs = append(errs, err)
	}
	if c.NetbirdToken, err = secret("NETBIRD_TOKEN", getenv, readFile); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(c.GroupPrefix) == "" {
		errs = append(errs, errors.New("GROUP_PREFIX is required: it marks the groups this tool owns"))
	}
	if c.LowercaseNames, err = boolean("GROUP_NAME_LOWERCASE", getenv("GROUP_NAME_LOWERCASE"), true); err != nil {
		errs = append(errs, err)
	}
	// Writing is opt-in: anything but an explicit "false" keeps the dry run.
	if c.DryRun, err = boolean("DRY_RUN", getenv("DRY_RUN"), true); err != nil {
		errs = append(errs, err)
	}
	if c.RunOnce, err = boolean("RUN_ONCE", getenv("RUN_ONCE"), false); err != nil {
		errs = append(errs, err)
	}

	c.Interval = defaultInterval
	if v := strings.TrimSpace(getenv("SYNC_INTERVAL")); v != "" {
		d, perr := time.ParseDuration(v)
		switch {
		case perr != nil:
			errs = append(errs, fmt.Errorf("SYNC_INTERVAL: %w", perr))
		case d < minInterval:
			errs = append(errs, fmt.Errorf("SYNC_INTERVAL must be at least %s", minInterval))
		default:
			c.Interval = d
		}
	}

	c.MaxRemovals = defaultMaxRemovals
	if v := strings.TrimSpace(getenv("MAX_REMOVALS")); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			errs = append(errs, errors.New("MAX_REMOVALS must be a non-negative integer (0 disables the limit)"))
		} else {
			c.MaxRemovals = n
		}
	}

	return c, errors.Join(errs...)
}

// secret reads NAME, or the file named by NAME_FILE (the usual way to mount a
// Kubernetes secret).
func secret(name string, getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v, nil
	}
	path := strings.TrimSpace(getenv(name + "_FILE"))
	if path == "" {
		return "", fmt.Errorf("%s or %s_FILE is required", name, name)
	}
	b, err := readFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s_FILE is empty", name)
	}
	return v, nil
}

// baseURL accepts https only. Plain http is allowed for loopback hosts so the
// tool can be pointed at a local test server; a token never travels in clear
// text to anything else.
func baseURL(name, value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s is not a valid URL", name)
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("%s must use https", name)
	}
	return strings.TrimRight(value, "/"), nil
}

func boolean(name, value string, fallback bool) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fallback, fmt.Errorf("%s must be true or false", name)
	}
	return b, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
