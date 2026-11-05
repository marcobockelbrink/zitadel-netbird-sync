package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/netbird"
	"github.com/marcobockelbrink/zitadel-netbird-sync/internal/zitadel"
)

var testOpt = Options{Prefix: "idp-", Lowercase: true, Exclude: []string{"tools"}}

func snapshot() Input {
	return Input{
		Projects: []zitadel.Project{{ID: "p1", Name: "Alpha"}, {ID: "p2", Name: "Beta"}, {ID: "p3", Name: "Tools"}},
		Grants: []zitadel.Grant{
			{UserID: "z1", ProjectID: "p1"},
			{UserID: "z2", ProjectID: "p1"},
			{UserID: "z2", ProjectID: "p2"},
			{UserID: "z1", ProjectID: "p3"},
			{UserID: "z-unverified", ProjectID: "p1"},
		},
		Emails: map[string]string{"z1": "Ann@Example.org", "z2": "bob@example.org"},
		Groups: []netbird.Group{
			{ID: "g-hand", Name: "team-alpha"},
			{ID: "g-alpha", Name: "idp-alpha"},
			{ID: "g-old", Name: "idp-gone"},
		},
		Users: []netbird.User{
			{ID: "n1", Email: "ann@example.org", Role: "admin", AutoGroups: []string{"g-hand", "g-old"}},
			{ID: "n2", Email: "bob@example.org", Role: "user", AutoGroups: []string{"g-alpha"}},
			{ID: "n3", Email: "carol@example.org", Role: "user", AutoGroups: []string{"g-alpha", "g-hand"}},
			{ID: "svc", IsServiceUser: true, AutoGroups: []string{"g-alpha"}},
			{ID: "n5"},
		},
	}
}

func TestBuildPlan(t *testing.T) {
	plan, err := BuildPlan(snapshot(), testOpt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"idp-beta"}; !reflect.DeepEqual(plan.CreateGroups, want) {
		t.Errorf("CreateGroups = %v, want %v", plan.CreateGroups, want)
	}
	if want := []string{"idp-gone"}; !reflect.DeepEqual(plan.Orphaned, want) {
		t.Errorf("Orphaned = %v, want %v", plan.Orphaned, want)
	}
	if plan.UsersWithoutEmail != 1 {
		t.Errorf("UsersWithoutEmail = %d, want 1", plan.UsersWithoutEmail)
	}
	if plan.Removals() != 2 {
		t.Errorf("Removals = %d, want 2", plan.Removals())
	}

	type change struct{ add, remove, managed []string }
	got := map[string]change{}
	for _, u := range plan.Updates {
		got[u.User.ID] = change{u.Add, u.Remove, u.Managed}
	}
	want := map[string]change{
		// Email match ignores case; the orphaned group is taken away.
		"n1": {add: []string{"idp-alpha"}, remove: []string{"idp-gone"}, managed: []string{"idp-alpha"}},
		"n2": {add: []string{"idp-beta"}, managed: []string{"idp-alpha", "idp-beta"}},
		// No grant in Zitadel: owned membership goes, nothing else.
		"n3": {remove: []string{"idp-alpha"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("updates = %+v\nwant      %+v", got, want)
	}
}

// A project nobody in NetBird belongs to gets no group, unless asked for.
func TestBuildPlanSkipsEmptyGroups(t *testing.T) {
	in := snapshot()
	in.Projects = append(in.Projects, zitadel.Project{ID: "p4", Name: "Gamma"}, zitadel.Project{ID: "p5", Name: "Delta"})
	// Dave holds a grant on Gamma but has never logged in to NetBird.
	in.Grants = append(in.Grants, zitadel.Grant{UserID: "z3", ProjectID: "p4"})
	in.Emails["z3"] = "dave@example.org"

	plan, err := BuildPlan(in, testOpt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"idp-beta"}; !reflect.DeepEqual(plan.CreateGroups, want) {
		t.Errorf("CreateGroups = %v, want %v", plan.CreateGroups, want)
	}

	opt := testOpt
	opt.CreateEmpty = true
	plan, err = BuildPlan(in, opt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"idp-beta", "idp-delta", "idp-gamma"}; !reflect.DeepEqual(plan.CreateGroups, want) {
		t.Errorf("with CreateEmpty: CreateGroups = %v, want %v", plan.CreateGroups, want)
	}
}

func TestBuildPlanRefusesDangerousInput(t *testing.T) {
	in := snapshot()

	if _, err := BuildPlan(in, Options{Prefix: " "}); err == nil {
		t.Error("empty prefix: want error")
	}

	empty := in
	empty.Projects = nil
	if _, err := BuildPlan(empty, testOpt); err == nil {
		t.Error("no projects: want error, an empty answer must not strip memberships")
	}

	dup := in
	dup.Groups = append(append([]netbird.Group{}, in.Groups...), netbird.Group{ID: "g-dup", Name: "idp-alpha"})
	if _, err := BuildPlan(dup, testOpt); err == nil {
		t.Error("duplicate owned group name: want error")
	}
}

func TestBuildPlanInclude(t *testing.T) {
	opt := Options{Prefix: "idp-", Lowercase: false, Include: []string{"beta"}}
	plan, err := BuildPlan(snapshot(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"idp-Beta"}; !reflect.DeepEqual(plan.CreateGroups, want) {
		t.Errorf("CreateGroups = %v, want %v", plan.CreateGroups, want)
	}
}

type fakeWriter struct {
	created []string
	sets    map[string][]string
	sent    map[string]netbird.User
	failFor string
	// readFailFor makes the read before the write fail for this user.
	readFailFor string
	// current is NetBird's state at write time, which may differ from the
	// snapshot the plan was built from.
	current map[string]netbird.User
}

// newFakeWriter starts with NetBird's current state equal to the snapshot.
func newFakeWriter(in Input) *fakeWriter {
	f := &fakeWriter{current: map[string]netbird.User{}}
	for _, u := range in.Users {
		f.current[u.ID] = u
	}
	return f
}

func (f *fakeWriter) User(_ context.Context, id string) (netbird.User, error) {
	if id == f.readFailFor {
		return netbird.User{}, errors.New("read boom")
	}
	u, ok := f.current[id]
	if !ok {
		return netbird.User{}, netbird.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeWriter) CreateGroup(_ context.Context, name string) (netbird.Group, error) {
	f.created = append(f.created, name)
	return netbird.Group{ID: "new-" + name, Name: name}, nil
}

func (f *fakeWriter) SetUserGroups(_ context.Context, u netbird.User, ids []string) error {
	if u.ID == f.failFor {
		return errors.New("boom")
	}
	if f.sets == nil {
		f.sets = map[string][]string{}
	}
	f.sets[u.ID] = ids
	if f.sent == nil {
		f.sent = map[string]netbird.User{}
	}
	f.sent[u.ID] = u
	return nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestApplyKeepsForeignGroups(t *testing.T) {
	in := snapshot()
	plan, err := BuildPlan(in, testOpt)
	if err != nil {
		t.Fatal(err)
	}
	w := newFakeWriter(in)
	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err != nil {
		t.Fatal(err)
	}
	if want := []string{"idp-beta"}; !reflect.DeepEqual(w.created, want) {
		t.Errorf("created = %v, want %v", w.created, want)
	}
	want := map[string][]string{
		"n1": {"g-hand", "g-alpha"},
		"n2": {"g-alpha", "new-idp-beta"},
		"n3": {"g-hand"},
	}
	if !reflect.DeepEqual(w.sets, want) {
		t.Errorf("sets = %v, want %v", w.sets, want)
	}
}

func TestApplyContinuesAfterUserFailure(t *testing.T) {
	in := snapshot()
	plan, _ := BuildPlan(in, testOpt)
	w := newFakeWriter(in)
	w.failFor = "n1"
	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err == nil {
		t.Fatal("want error for the failed update")
	}
	if len(w.sets) != 2 {
		t.Errorf("updated %d users after one failure, want 2", len(w.sets))
	}
}

// Whatever names and addresses arrive, the plan must stay inside the prefix:
// it may only create, add and remove groups it owns.
func FuzzBuildPlanStaysInsidePrefix(f *testing.F) {
	f.Add("idp-", "Alpha", "team-alpha", "ann@example.org", true)
	f.Add("x", "", "x", "", false)
	f.Add("idp-", "idp-", "idp-", " ANN@example.org ", true)
	f.Fuzz(func(t *testing.T, prefix, project, foreign, email string, lower bool) {
		in := Input{
			Projects: []zitadel.Project{{ID: "p1", Name: project}, {ID: "p2", Name: "Other"}},
			Grants:   []zitadel.Grant{{UserID: "z1", ProjectID: "p1"}},
			Emails:   map[string]string{"z1": email},
			Groups:   []netbird.Group{{ID: "g-foreign", Name: foreign}, {ID: "g-own", Name: prefix + "old"}},
			Users: []netbird.User{
				{ID: "n1", Email: email, AutoGroups: []string{"g-foreign", "g-own"}},
				{ID: "n2", Email: "someone@example.org", AutoGroups: []string{"g-foreign"}},
			},
		}
		opt := Options{Prefix: prefix, Lowercase: lower}
		plan, err := BuildPlan(in, opt)
		if err != nil {
			return
		}
		owned := func(name string) bool { return strings.HasPrefix(name, prefix) }
		for _, name := range plan.CreateGroups {
			if !owned(name) {
				t.Fatalf("would create %q outside prefix %q", name, prefix)
			}
		}
		for _, u := range plan.Updates {
			for _, name := range append(append([]string{}, u.Add...), u.Remove...) {
				if !owned(name) {
					t.Fatalf("would change membership of %q outside prefix %q", name, prefix)
				}
			}
		}

		// Applying the plan must keep every group the sync does not own.
		w := newFakeWriter(in)
		if err := Apply(context.Background(), w, in, plan, opt, quiet); err != nil {
			t.Fatal(err)
		}
		if !owned(foreign) {
			for id, ids := range w.sets {
				if !slices.Contains(ids, "g-foreign") {
					t.Fatalf("user %s lost a foreign group: %v", id, ids)
				}
			}
		}
	})
}

// A second run over the state the first run produced must change nothing.
func TestPlanIsStableAfterApply(t *testing.T) {
	in := snapshot()
	plan, _ := BuildPlan(in, testOpt)
	w := newFakeWriter(in)
	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err != nil {
		t.Fatal(err)
	}
	in.Groups = append(in.Groups, netbird.Group{ID: "new-idp-beta", Name: "idp-beta"})
	for i := range in.Users {
		if ids, ok := w.sets[in.Users[i].ID]; ok {
			in.Users[i].AutoGroups = ids
		}
	}
	again, err := BuildPlan(in, testOpt)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Errorf("second plan not empty: %+v", again)
	}
}

// The collective group holds everyone who is in at least one synced project.
func TestBuildPlanAllGroup(t *testing.T) {
	in := snapshot()
	in.Groups = append(in.Groups, netbird.Group{ID: "g-all", Name: "idp-all"})
	in.Users[2].AutoGroups = append(in.Users[2].AutoGroups, "g-all") // carol: no grant any more
	in.Users[1].AutoGroups = append(in.Users[1].AutoGroups, "g-all") // bob: already in
	opt := testOpt
	opt.AllGroup = "All"

	plan, err := BuildPlan(in, opt)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"idp-beta"}; !reflect.DeepEqual(plan.CreateGroups, want) {
		t.Errorf("CreateGroups = %v, want %v", plan.CreateGroups, want)
	}
	if len(plan.Orphaned) != 1 || plan.Orphaned[0] != "idp-gone" {
		t.Errorf("Orphaned = %v, the collective group must not count", plan.Orphaned)
	}
	got := map[string]UserUpdate{}
	for _, u := range plan.Updates {
		got[u.User.ID] = u
	}
	if want := []string{"idp-all", "idp-alpha"}; !reflect.DeepEqual(got["n1"].Add, want) {
		t.Errorf("n1 add = %v, want %v", got["n1"].Add, want)
	}
	if want := []string{"idp-all", "idp-alpha", "idp-beta"}; !reflect.DeepEqual(got["n2"].Managed, want) {
		t.Errorf("n2 managed = %v, want %v", got["n2"].Managed, want)
	}
	if want := []string{"idp-all", "idp-alpha"}; !reflect.DeepEqual(got["n3"].Remove, want) {
		t.Errorf("n3 remove = %v, want %v", got["n3"].Remove, want)
	}
}

// A user whose only grant is on an excluded project is in no synced project,
// so the collective group does not take them either.
func TestBuildPlanAllGroupIgnoresExcludedProjects(t *testing.T) {
	in := snapshot()
	in.Grants = []zitadel.Grant{{UserID: "z1", ProjectID: "p3"}}
	opt := testOpt
	opt.AllGroup = "all"

	plan, err := BuildPlan(in, opt)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.CreateGroups, "idp-all") {
		t.Errorf("CreateGroups = %v, nobody belongs in idp-all", plan.CreateGroups)
	}
}

// The collective group is only created once it has a member.
func TestBuildPlanAllGroupNeedsMember(t *testing.T) {
	in := snapshot()
	in.Grants = nil
	opt := testOpt
	opt.AllGroup = "all"

	plan, err := BuildPlan(in, opt)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.CreateGroups, "idp-all") {
		t.Errorf("CreateGroups = %v, idp-all would be empty", plan.CreateGroups)
	}
}

// A project that maps onto the collective group's name must stop the run,
// otherwise the two would silently share one group.
func TestBuildPlanAllGroupCollision(t *testing.T) {
	in := snapshot()
	in.Projects = append(in.Projects, zitadel.Project{ID: "p9", Name: "All"})
	opt := testOpt
	opt.AllGroup = "all"

	if _, err := BuildPlan(in, opt); err == nil {
		t.Fatal("expected an error for a project named like the collective group")
	}
	// Without the feature the same project is an ordinary one.
	if _, err := BuildPlan(in, testOpt); err != nil {
		t.Fatalf("no AllGroup: %v", err)
	}
}

// An admin may block, demote or regroup a user while a run is under way. The
// write must start from NetBird's state at that moment, not from the snapshot:
// it must never unblock, re-promote or re-add a hand-kept group.
func TestApplyUsesCurrentUserStateNotSnapshot(t *testing.T) {
	in := snapshot()
	plan, err := BuildPlan(in, testOpt)
	if err != nil {
		t.Fatal(err)
	}
	w := newFakeWriter(in)
	// After the snapshot: n1 (admin, in team-alpha) was blocked, demoted and
	// taken out of team-alpha by hand.
	w.current["n1"] = netbird.User{ID: "n1", Email: "ann@example.org", Role: "user", IsBlocked: true, AutoGroups: []string{"g-old"}}
	// n3 was deleted.
	delete(w.current, "n3")

	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err != nil {
		t.Fatal(err)
	}
	sent := w.sent["n1"]
	if !sent.IsBlocked || sent.Role != "user" {
		t.Errorf("n1 sent with role=%q blocked=%v, want the current role=user blocked=true", sent.Role, sent.IsBlocked)
	}
	if ids := w.sets["n1"]; !reflect.DeepEqual(ids, []string{"g-alpha"}) {
		t.Errorf("n1 groups = %v, want [g-alpha] (team-alpha must not come back)", ids)
	}
	if _, ok := w.sets["n3"]; ok {
		t.Error("n3 no longer exists and must not be written")
	}
}

// A failed read is a failed update: nothing is written for that user, the
// others go on, and the run reports the failure.
func TestApplyCountsFailedReadAsFailure(t *testing.T) {
	in := snapshot()
	plan, _ := BuildPlan(in, testOpt)
	w := newFakeWriter(in)
	w.readFailFor = "n1"
	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err == nil {
		t.Fatal("want error for the failed read")
	}
	if _, ok := w.sets["n1"]; ok {
		t.Error("n1 must not be written after a failed read")
	}
	if len(w.sets) != 2 {
		t.Errorf("updated %d users, want the other 2", len(w.sets))
	}
}

// A hand-kept group added after the snapshot survives the write.
func TestApplyKeepsHandGroupAddedDuringRun(t *testing.T) {
	in := snapshot()
	plan, _ := BuildPlan(in, testOpt)
	w := newFakeWriter(in)
	n2 := w.current["n2"]
	n2.AutoGroups = []string{"g-alpha", "g-new-hand"}
	w.current["n2"] = n2
	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err != nil {
		t.Fatal(err)
	}
	if ids := w.sets["n2"]; !slices.Contains(ids, "g-new-hand") {
		t.Errorf("n2 groups = %v, want g-new-hand kept", ids)
	}
}

// After cancellation nothing more is attempted.
func TestApplyStopsWhenCancelled(t *testing.T) {
	in := snapshot()
	plan, _ := BuildPlan(in, testOpt)
	w := newFakeWriter(in)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Apply(ctx, w, in, plan, testOpt, quiet); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(w.sets) != 0 {
		t.Errorf("wrote %d users after cancellation, want 0", len(w.sets))
	}
}

// A NetBird user whose email is missing cannot be matched to any grant, so it
// is entitled to no owned group. Revocation must still reach it; groups the
// sync does not own stay untouched.
func TestBuildPlanRevokesOwnedGroupsFromUsersWithoutEmail(t *testing.T) {
	in := snapshot()
	in.Users = append(in.Users,
		netbird.User{ID: "n6", Email: "", AutoGroups: []string{"g-hand", "g-alpha"}},
		netbird.User{ID: "n7", Email: "  ", AutoGroups: []string{"g-old"}},
	)
	plan, err := BuildPlan(in, testOpt)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, u := range plan.Updates {
		if u.User.ID == "n6" || u.User.ID == "n7" {
			if len(u.Add) != 0 || len(u.Managed) != 0 {
				t.Errorf("%s: add=%v managed=%v, want none", u.User.ID, u.Add, u.Managed)
			}
			got[u.User.ID] = u.Remove
		}
	}
	want := map[string][]string{"n6": {"idp-alpha"}, "n7": {"idp-gone"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("removals = %v, want %v", got, want)
	}
	if plan.UsersWithoutEmail != 3 { // n5, n6, n7
		t.Errorf("UsersWithoutEmail = %d, want 3", plan.UsersWithoutEmail)
	}

	w := newFakeWriter(in)
	if err := Apply(context.Background(), w, in, plan, testOpt, quiet); err != nil {
		t.Fatal(err)
	}
	if ids := w.sets["n6"]; !reflect.DeepEqual(ids, []string{"g-hand"}) {
		t.Errorf("n6 groups after apply = %v, want [g-hand]", ids)
	}
	if ids, ok := w.sets["n7"]; !ok || len(ids) != 0 {
		t.Errorf("n7 groups after apply = %v, want []", ids)
	}
}

// If NetBird suddenly reports no address for anyone (a renamed field, a
// changed setting), every membership would go. That is doubtful input, like
// an empty project list, and must stop the run.
func TestBuildPlanRefusesWhenNoUserHasAnEmail(t *testing.T) {
	in := snapshot()
	for i := range in.Users {
		in.Users[i].Email = ""
	}
	if _, err := BuildPlan(in, testOpt); err == nil {
		t.Error("no NetBird user with an address: want error")
	}
}
