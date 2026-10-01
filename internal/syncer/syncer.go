// Package syncer works out which NetBird groups and memberships follow from
// the Zitadel projects and grants, and applies the difference.
//
// Ownership rule: a group belongs to the sync if and only if its name starts
// with the configured prefix. Everything else in the account is left alone.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/netbird"
	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/zitadel"
)

// Options control naming and project selection.
type Options struct {
	Prefix    string
	Lowercase bool
	// Include, when not empty, limits the sync to these project names.
	Include []string
	// Exclude removes project names, also from an Include list.
	Exclude []string
	// CreateEmpty also creates groups for projects that no NetBird user
	// belongs to. By default a group appears with its first member.
	CreateEmpty bool
}

// Input is one consistent snapshot of both systems.
type Input struct {
	Projects []zitadel.Project
	Grants   []zitadel.Grant
	// Emails maps Zitadel user ID to the verified address of an active user.
	Emails map[string]string
	Groups []netbird.Group
	Users  []netbird.User
}

// UserUpdate is the change for one NetBird user, in group names.
type UserUpdate struct {
	User   netbird.User
	Add    []string
	Remove []string
	// Managed is the full set of owned groups the user should end up in.
	Managed []string
}

// Plan is the difference between the snapshot and the desired state.
type Plan struct {
	CreateGroups []string
	Updates      []UserUpdate
	// Orphaned are owned groups without a matching project. They are emptied
	// by the updates above but never deleted.
	Orphaned []string
	// SkippedUsers counts NetBird users without an email address.
	SkippedUsers int
}

// Removals is the number of memberships the plan takes away.
func (p Plan) Removals() int {
	n := 0
	for _, u := range p.Updates {
		n += len(u.Remove)
	}
	return n
}

// Empty reports whether applying the plan would change nothing.
func (p Plan) Empty() bool { return len(p.CreateGroups) == 0 && len(p.Updates) == 0 }

// BuildPlan compares the snapshot with the desired state.
func BuildPlan(in Input, opt Options) (Plan, error) {
	if strings.TrimSpace(opt.Prefix) == "" {
		return Plan{}, errors.New("empty group prefix: refusing to run without an ownership marker")
	}
	if len(in.Projects) == 0 {
		// An empty answer is far more likely a permission or outage problem
		// than a real empty organization, and acting on it would strip every
		// membership.
		return Plan{}, errors.New("zitadel returned no projects: refusing to remove all memberships")
	}

	// Desired groups: one per selected project.
	groupOfProject := map[string]string{}
	desiredGroups := map[string]bool{}
	for _, p := range in.Projects {
		if !selected(p.Name, opt) {
			continue
		}
		name := groupName(p.Name, opt)
		groupOfProject[p.ID] = name
		desiredGroups[name] = true
	}

	// Existing groups, and which of them are owned.
	idOfGroup := map[string]string{}
	nameOfOwnedID := map[string]string{}
	for _, g := range in.Groups {
		if !strings.HasPrefix(g.Name, opt.Prefix) {
			continue
		}
		if _, dup := idOfGroup[g.Name]; dup {
			return Plan{}, fmt.Errorf("netbird has more than one group named %q: resolve the duplicate first", g.Name)
		}
		idOfGroup[g.Name] = g.ID
		nameOfOwnedID[g.ID] = g.Name
	}

	// Desired membership per email address.
	want := map[string]map[string]bool{}
	for _, g := range in.Grants {
		group, ok := groupOfProject[g.ProjectID]
		if !ok {
			continue
		}
		email := normalize(in.Emails[g.UserID])
		if email == "" {
			continue
		}
		if want[email] == nil {
			want[email] = map[string]bool{}
		}
		want[email][group] = true
	}

	var plan Plan
	for name := range idOfGroup {
		if !desiredGroups[name] {
			plan.Orphaned = append(plan.Orphaned, name)
		}
	}

	// populated are the desired groups at least one NetBird user belongs to.
	populated := map[string]bool{}
	for _, u := range in.Users {
		if u.IsServiceUser {
			continue
		}
		email := normalize(u.Email)
		if email == "" {
			plan.SkippedUsers++
			continue
		}
		have := map[string]bool{}
		for _, id := range u.AutoGroups {
			if name, owned := nameOfOwnedID[id]; owned {
				have[name] = true
			}
		}
		upd := UserUpdate{User: u}
		for name := range want[email] {
			populated[name] = true
			upd.Managed = append(upd.Managed, name)
			if !have[name] {
				upd.Add = append(upd.Add, name)
			}
		}
		for name := range have {
			if !want[email][name] {
				upd.Remove = append(upd.Remove, name)
			}
		}
		if len(upd.Add) == 0 && len(upd.Remove) == 0 {
			continue
		}
		sort.Strings(upd.Add)
		sort.Strings(upd.Remove)
		sort.Strings(upd.Managed)
		plan.Updates = append(plan.Updates, upd)
	}

	for name := range desiredGroups {
		if _, exists := idOfGroup[name]; !exists && (opt.CreateEmpty || populated[name]) {
			plan.CreateGroups = append(plan.CreateGroups, name)
		}
	}

	sort.Strings(plan.CreateGroups)
	sort.Strings(plan.Orphaned)
	sort.Slice(plan.Updates, func(i, j int) bool { return plan.Updates[i].User.ID < plan.Updates[j].User.ID })
	return plan, nil
}

// Writer is the part of the NetBird client that Apply needs.
type Writer interface {
	CreateGroup(ctx context.Context, name string) (netbird.Group, error)
	SetUserGroups(ctx context.Context, u netbird.User, groupIDs []string) error
}

// Apply carries out the plan. A failed group creation stops the run, because
// later updates would refer to a group that does not exist. A failed user
// update is logged and the run continues; the next run retries it.
func Apply(ctx context.Context, w Writer, in Input, plan Plan, opt Options, log *slog.Logger) error {
	idOfGroup := map[string]string{}
	owned := map[string]bool{}
	for _, g := range in.Groups {
		if strings.HasPrefix(g.Name, opt.Prefix) {
			idOfGroup[g.Name] = g.ID
			owned[g.ID] = true
		}
	}
	for _, name := range plan.CreateGroups {
		g, err := w.CreateGroup(ctx, name)
		if err != nil {
			return fmt.Errorf("create group %q: %w", name, err)
		}
		idOfGroup[name] = g.ID
		owned[g.ID] = true
		log.Info("group created", "group", name)
	}

	var failed int
	for _, upd := range plan.Updates {
		// Keep every group the sync does not own, in its existing order.
		ids := make([]string, 0, len(upd.User.AutoGroups)+len(upd.Managed))
		for _, id := range upd.User.AutoGroups {
			if !owned[id] {
				ids = append(ids, id)
			}
		}
		for _, name := range upd.Managed {
			id, ok := idOfGroup[name]
			if !ok {
				return fmt.Errorf("group %q has no id: plan and snapshot disagree", name)
			}
			ids = append(ids, id)
		}
		if err := w.SetUserGroups(ctx, upd.User, ids); err != nil {
			failed++
			log.Error("user update failed", "user_id", upd.User.ID, "error", err.Error())
			continue
		}
		log.Info("user updated", "user_id", upd.User.ID, "added", upd.Add, "removed", upd.Remove)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d user updates failed", failed, len(plan.Updates))
	}
	return nil
}

func groupName(project string, opt Options) string {
	name := strings.TrimSpace(project)
	if opt.Lowercase {
		name = strings.ToLower(name)
	}
	return opt.Prefix + name
}

func selected(project string, opt Options) bool {
	if contains(opt.Exclude, project) {
		return false
	}
	return len(opt.Include) == 0 || contains(opt.Include, project)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

func normalize(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
