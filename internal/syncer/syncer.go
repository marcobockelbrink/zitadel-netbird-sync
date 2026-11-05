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
	"unicode/utf8"

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
	// AllGroup, when not empty, names a collective group (after the prefix)
	// that holds every user who is in at least one synced project. NetBird
	// has no nested groups, so this is the union, kept by the sync itself.
	AllGroup string
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
	// UsersWithoutEmail counts non-service NetBird users without an email
	// address. They get no owned group, and lose any they hold.
	UsersWithoutEmail int
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
	// Two projects must never feed one group: members of one would gain the
	// other's access.
	groupOfProject := map[string]string{}
	projectOfGroup := map[string]string{}
	desiredGroups := map[string]bool{}
	for _, p := range in.Projects {
		if !selected(p.Name, opt) {
			continue
		}
		name := groupName(p.Name, opt)
		if other, taken := projectOfGroup[name]; taken {
			return Plan{}, fmt.Errorf("projects %s and %s both map to group %q: rename one or exclude it", other, p.ID, name)
		}
		projectOfGroup[name] = p.ID
		groupOfProject[p.ID] = name
		desiredGroups[name] = true
	}

	// One address must stand for one person, or two accounts would share the
	// grants. Errors name IDs only, never addresses.
	netbirdOfEmail := map[string]string{}
	for _, u := range in.Users {
		email := normalize(u.Email)
		if u.IsServiceUser || email == "" {
			continue
		}
		if other, dup := netbirdOfEmail[email]; dup {
			return Plan{}, fmt.Errorf("netbird users %s and %s share one email address: resolve it first", other, u.ID)
		}
		netbirdOfEmail[email] = u.ID
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

	// The collective group is an owned group like the others, so it needs no
	// special case further down. It must not share a name with a project.
	allGroup := ""
	if strings.TrimSpace(opt.AllGroup) != "" {
		allGroup = groupName(opt.AllGroup, opt)
		if desiredGroups[allGroup] {
			return Plan{}, fmt.Errorf("a project maps to the collective group %q: rename it or change ALL_GROUP", allGroup)
		}
		desiredGroups[allGroup] = true
	}

	// Desired membership per email address.
	zitadelOfEmail := map[string]string{}
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
		// Only users with a synced grant matter; a duplicate elsewhere in the
		// directory changes nothing and must not stop the run.
		if other, dup := zitadelOfEmail[email]; dup && other != g.UserID {
			a, b := min(other, g.UserID), max(other, g.UserID)
			return Plan{}, fmt.Errorf("zitadel users %s and %s share one email address: resolve it first", a, b)
		}
		zitadelOfEmail[email] = g.UserID
		if want[email] == nil {
			want[email] = map[string]bool{}
		}
		want[email][group] = true
		if allGroup != "" {
			want[email][allGroup] = true
		}
	}

	var plan Plan
	for name := range idOfGroup {
		if !desiredGroups[name] {
			plan.Orphaned = append(plan.Orphaned, name)
		}
	}

	// populated are the desired groups at least one NetBird user belongs to.
	populated := map[string]bool{}
	humans := 0
	for _, u := range in.Users {
		if u.IsServiceUser {
			continue
		}
		humans++
		// A user without an address matches no grant, so it is entitled to no
		// owned group. It still goes through the loop: what it holds is taken
		// away, otherwise revocation would never reach it.
		email := normalize(u.Email)
		if email == "" {
			plan.UsersWithoutEmail++
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

	// No address at all is far more likely a changed API or setting than a
	// real account, and acting on it would strip every membership.
	if humans > 0 && plan.UsersWithoutEmail == humans {
		return Plan{}, errors.New("netbird returned no user with an email address: refusing to remove all memberships")
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
	// User returns a user's current state, or netbird.ErrUserNotFound.
	User(ctx context.Context, id string) (netbird.User, error)
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if err := ctx.Err(); err != nil {
			return err
		}
		// The snapshot may be minutes old. An admin may have blocked, demoted
		// or regrouped the user since; the write must keep that, so it starts
		// from the user's current state.
		cur, err := w.User(ctx, upd.User.ID)
		if errors.Is(err, netbird.ErrUserNotFound) {
			log.Info("user no longer exists, skipped", "user_id", upd.User.ID)
			continue
		}
		if err != nil {
			failed++
			log.Error("user read failed", "user_id", upd.User.ID, "error", err.Error())
			continue
		}
		// Keep every group the sync does not own, in its existing order. An
		// owned group created by hand after the snapshot counts as foreign
		// here; the next run sees it.
		ids := make([]string, 0, len(cur.AutoGroups)+len(upd.Managed))
		for _, id := range cur.AutoGroups {
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
		if err := w.SetUserGroups(ctx, cur, ids); err != nil {
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

// normalize folds ASCII case only. An address with any other character is
// treated as missing: Unicode case folding maps distinct addresses onto one
// (KELVIN SIGN U+212A lowercases to "k").
func normalize(email string) string {
	email = strings.TrimSpace(email)
	for i := 0; i < len(email); i++ {
		if email[i] >= utf8.RuneSelf {
			return ""
		}
	}
	return strings.ToLower(email)
}
