package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// The build table is written only when every member's agent says it knows
// it. 0.1.0's agent answers caps with mixed_os and backup only; an agent
// from before D37 answers with an error. Either one keeps the old rule.
func TestEveryMemberKnowsPGBuilds(t *testing.T) {
	var old capsResp
	if err := json.Unmarshal([]byte(`{"mixed_os":true,"backup":true}`), &old); err != nil {
		t.Fatal(err)
	}
	cur := capsResp{MixedOS: true, Backup: true, PGBuilds: true}
	has := func(r capsResp) bool { return r.PGBuilds }
	answers := map[string]func() (capsResp, error){}
	ask := func(_ context.Context, id string) (capsResp, error) {
		if f, ok := answers[id]; ok {
			return f()
		}
		t.Fatalf("asked %s", id)
		return capsResp{}, nil
	}
	ids := []string{"a", "b", "c"}
	answers["b"] = func() (capsResp, error) { return cur, nil }
	answers["c"] = func() (capsResp, error) { return old, nil }
	if who, ok := everyMemberKnows(context.Background(), ids, "a", ask, has); ok || who != "c" {
		t.Fatalf("with a 0.1.0 member: %q %v", who, ok)
	}
	answers["c"] = func() (capsResp, error) { return capsResp{}, errors.New("unknown agent method \"caps\"") }
	if who, ok := everyMemberKnows(context.Background(), ids, "a", ask, has); ok || who != "c" {
		t.Fatalf("with a member that does not answer: %q %v", who, ok)
	}
	answers["c"] = func() (capsResp, error) { return cur, nil }
	if _, ok := everyMemberKnows(context.Background(), ids, "a", ask, has); !ok {
		t.Fatal("every member knows it")
	}
	// What this agent answers.
	var got capsResp
	b, _ := json.Marshal(capsResp{MixedOS: true, Backup: true, PGBuilds: true})
	if err := json.Unmarshal(b, &got); err != nil || !got.PGBuilds || !strings.Contains(string(b), `"pg_builds":true`) {
		t.Fatalf("caps answer %s", b)
	}
}

func TestKnownPGBuildsTable(t *testing.T) {
	kb := knownPGBuilds()
	cur, err := pg.CurrentBuild()
	if err != nil {
		t.Fatal(err)
	}
	b, ok := kb[cur.SHA256]
	if !ok || b.Version != pg.Version {
		t.Fatalf("current build %s missing or wrong: %+v", cur.SHA256, b)
	}
	if len(kb) != len(pg.KnownBuilds()) {
		t.Fatalf("%d entries, %d builds", len(kb), len(pg.KnownBuilds()))
	}
	// Exactly what the state machine accepts.
	st := cluster.State{}
	reg := &cluster.RegisterPGBuilds{Builds: kb}
	if _, err := cluster.EncodeCommand(reg); err != nil {
		t.Fatal(err)
	}
	st.PGBuilds = kb
	for sha, b := range kb {
		if _, ok := st.PGVersionOf(cluster.Platform{OS: b.OS, Arch: b.Arch, PGBuildSHA: sha}); !ok {
			t.Errorf("%s %s/%s does not parse", b.Version, b.OS, b.Arch)
		}
	}
}

func TestSelectedBuild(t *testing.T) {
	cur, _ := pg.CurrentBuild()
	if b, err := selectedBuild(); err != nil || b != cur {
		t.Fatalf("default: %+v %v", b, err)
	}
	var earlier string
	for _, b := range pg.KnownBuilds() {
		if b.Version != pg.Version && b.Target == cur.Target {
			earlier = b.Version
			break
		}
	}
	if earlier == "" {
		t.Fatal("no earlier pinned release for this platform")
	}
	t.Setenv(testPGBuildEnv, earlier)
	if b, err := selectedBuild(); err != nil || b.Version != earlier || b.Target != cur.Target {
		t.Fatalf("%s: %+v %v", earlier, b, err)
	}
	t.Setenv(testPGBuildEnv, "17.0.0")
	if _, err := selectedBuild(); err == nil {
		t.Fatal("an unpinned release was accepted")
	}
}

func TestMinorSwitchBlocked(t *testing.T) {
	sha := func(c byte) string { return strings.Repeat(string(c), 64) }
	p186 := cluster.Platform{OS: "linux", Arch: "arm64", PGBuildSHA: sha('1')}
	p187 := cluster.Platform{OS: "linux", Arch: "arm64", PGBuildSHA: sha('2')}
	st := cluster.State{
		PrimaryID: "n1",
		Members: map[string]cluster.Member{
			"n1": {ID: "n1", Name: "n1", Role: cluster.RoleData, Platform: p186},
			"n2": {ID: "n2", Name: "n2", Role: cluster.RoleData, Platform: p187},
			"n3": {ID: "n3", Name: "n3", Role: cluster.RoleData, Platform: p186},
			"w":  {ID: "w", Name: "w", Role: cluster.RoleWitness},
		},
		PGBuilds: map[string]cluster.PGBuild{
			sha('1'): {Version: "18.6.0", OS: "linux", Arch: "arm64"},
			sha('2'): {Version: "18.7.0", OS: "linux", Arch: "arm64"},
		},
	}
	// n3 is still on 18.6: switching to n2 (18.7) would leave it unable
	// to copy the new main node.
	if why := minorSwitchBlocked(st, st.Members["n2"]); !strings.Contains(why, "newer than n3") {
		t.Fatalf("n3 not updated: %q", why)
	}
	// To n3 (the same minor as the main node): fine; n2 is newer.
	if why := minorSwitchBlocked(st, st.Members["n3"]); why != "" {
		t.Fatalf("to an older copy: %q", why)
	}
	// Once n3 is updated: the old main node n1 is the next to update, so
	// it does not block.
	m := st.Members["n3"]
	m.Platform = p187
	st.Members["n3"] = m
	if why := minorSwitchBlocked(st, st.Members["n2"]); why != "" {
		t.Fatalf("copies updated: %q", why)
	}
	// Without the table nothing is on a line: as before D70.
	st.PGBuilds = nil
	m.Platform = p186
	st.Members["n3"] = m
	if why := minorSwitchBlocked(st, st.Members["n2"]); why != "" {
		t.Fatalf("without the table: %q", why)
	}
}

// A target whose record still names the build it ran before its update
// (seconds after its start) is not switched to: the copies compare its
// record.
func TestMinorRecordPending(t *testing.T) {
	sha := func(c byte) string { return strings.Repeat(string(c), 64) }
	p186 := cluster.Platform{OS: "linux", Arch: "arm64", PGBuildSHA: sha('1')}
	p187 := cluster.Platform{OS: "linux", Arch: "arm64", PGBuildSHA: sha('2')}
	st := cluster.State{PGBuilds: map[string]cluster.PGBuild{
		sha('1'): {Version: "18.6.0", OS: "linux", Arch: "arm64"},
		sha('2'): {Version: "18.7.0", OS: "linux", Arch: "arm64"},
	}}
	target := cluster.Member{ID: "n2", Name: "n2", Role: cluster.RoleData, Platform: p186}
	if why := minorRecordPending(st, target, p187); !strings.Contains(why, "has not recorded") {
		t.Fatalf("stale record: %q", why)
	}
	if why := minorRecordPending(st, target, p186); why != "" {
		t.Fatalf("current record: %q", why)
	}
	// Without the table (a member on a release before D70) the record
	// never moves: no wait, as before.
	st.PGBuilds = nil
	if why := minorRecordPending(st, target, p187); why != "" {
		t.Fatalf("without the table: %q", why)
	}
}
