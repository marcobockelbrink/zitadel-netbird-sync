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
	if plan.SkippedUsers != 1 {
		t.Errorf("SkippedUsers = %d, want 1", plan.SkippedUsers)
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
	failFor string
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
	return nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestApplyKeepsForeignGroups(t *testing.T) {
	in := snapshot()
	plan, err := BuildPlan(in, testOpt)
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWriter{}
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
	w := &fakeWriter{failFor: "n1"}
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
		w := &fakeWriter{}
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
	w := &fakeWriter{}
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
