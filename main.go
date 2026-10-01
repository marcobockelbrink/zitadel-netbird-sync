// Command zitadel-netbird-sync mirrors Zitadel projects and their members into
// NetBird groups.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/config"
	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/netbird"
	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/syncer"
	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/zitadel"
)

const runTimeout = 5 * time.Minute

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		log.Error("invalid configuration", "error", err.Error())
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hc := &http.Client{Timeout: 30 * time.Second}
	zc := zitadel.New(cfg.ZitadelURL, cfg.ZitadelToken, cfg.ZitadelOrgID, hc)
	nc := netbird.New(cfg.NetbirdURL, cfg.NetbirdToken, hc)
	opt := syncer.Options{Prefix: cfg.GroupPrefix, Lowercase: cfg.LowercaseNames, Include: cfg.Include, Exclude: cfg.Exclude}

	log.Info("starting", "dry_run", cfg.DryRun, "interval", cfg.Interval.String(), "prefix", cfg.GroupPrefix, "run_once", cfg.RunOnce)

	if cfg.RunOnce {
		if err := run(ctx, cfg, zc, nc, opt, log); err != nil {
			log.Error("sync failed", "error", err.Error())
			os.Exit(1)
		}
		return
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := run(ctx, cfg, zc, nc, opt, log); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sync failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return
		case <-ticker.C:
		}
	}
}

// run performs one full comparison. It keeps no state between runs.
func run(parent context.Context, cfg config.Config, zc *zitadel.Client, nc *netbird.Client, opt syncer.Options, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(parent, runTimeout)
	defer cancel()

	var in syncer.Input
	var err error
	if in.Projects, err = zc.ActiveProjects(ctx); err != nil {
		return err
	}
	if in.Grants, err = zc.ActiveGrants(ctx); err != nil {
		return err
	}
	if in.Emails, err = zc.ActiveUserEmails(ctx); err != nil {
		return err
	}
	if in.Groups, err = nc.Groups(ctx); err != nil {
		return err
	}
	if in.Users, err = nc.Users(ctx); err != nil {
		return err
	}

	plan, err := syncer.BuildPlan(in, opt)
	if err != nil {
		return err
	}

	for _, name := range plan.Orphaned {
		log.Warn("owned group has no matching project; members are removed, the group is kept", "group", name)
	}
	if plan.SkippedUsers > 0 {
		log.Info("netbird users without an email address were skipped", "count", plan.SkippedUsers)
	}
	log.Info("plan",
		"projects", len(in.Projects), "grants", len(in.Grants), "netbird_users", len(in.Users),
		"groups_to_create", len(plan.CreateGroups), "users_to_update", len(plan.Updates), "removals", plan.Removals())

	if plan.Empty() {
		return nil
	}
	if cfg.MaxRemovals > 0 && plan.Removals() > cfg.MaxRemovals {
		return fmt.Errorf("plan removes %d memberships, more than MAX_REMOVALS=%d: nothing was changed", plan.Removals(), cfg.MaxRemovals)
	}

	if cfg.DryRun {
		for _, name := range plan.CreateGroups {
			log.Info("dry run: would create group", "group", name)
		}
		for _, u := range plan.Updates {
			log.Info("dry run: would update user", "user_id", u.User.ID, "add", u.Add, "remove", u.Remove)
		}
		return nil
	}
	return syncer.Apply(ctx, nc, in, plan, opt, log)
}
