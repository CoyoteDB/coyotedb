package agent

// PostgreSQL minor updates (D70, safety rule 1). The state machine decides
// which builds may share the cluster (cluster.checkPlatform with the
// cluster's table of pinned builds, State.PGBuilds). This file is the
// agent's side:
//
//   - registerPGBuilds: every data node adds the builds its own program
//     pins (pg.KnownBuilds) to the cluster's table, once every member's
//     agent knows the command (caps "pg_builds"), so an older member's
//     strict decoder never meets it during a rolling update.
//   - minorFollowBlocked: a copy never copies a main node that runs a
//     newer PostgreSQL minor than its own program (PostgreSQL's rule:
//     standbys first; a newer primary may write WAL an older standby cannot
//     read). It stops its database and waits for its own update.
//   - minorSwitchBlocked: a planned switch never makes a node the main node
//     while another copy runs an older PostgreSQL than it (that copy could
//     not follow); the current main node is the exception, since it is the
//     next one to update in the documented order.

import (
	"context"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// testPGBuildEnv (tests only): run this earlier pinned PostgreSQL release
// (pg.KnownBuilds) instead of the current one, as an older CoyoteDB
// release would, so an update across a PostgreSQL minor can be exercised
// on one machine. Never set in normal use.
const testPGBuildEnv = "COYOTE_TEST_PG_BUILD"

// pgBuildsState is the registration's memory.
type pgBuildsState struct {
	mu      sync.Mutex
	capsKey string
	capsAt  time.Time
}

// knownPGBuilds is this program's pinned table in the cluster's form.
func knownPGBuilds() map[string]cluster.PGBuild {
	out := map[string]cluster.PGBuild{}
	for _, b := range pg.KnownBuilds() {
		goos, goarch, ok := pg.GoPlatform(b.Target)
		if !ok {
			continue
		}
		out[b.SHA256] = cluster.PGBuild{Version: b.Version, OS: goos, Arch: goarch}
	}
	return out
}

// selectedBuild is the build this node runs: the current pinned one, or
// (tests only) the earlier pinned release testPGBuildEnv names.
func selectedBuild() (pg.Build, error) {
	v := os.Getenv(testPGBuildEnv)
	cur, err := pg.CurrentBuild()
	if v == "" || err != nil {
		return cur, err
	}
	for _, b := range pg.KnownBuilds() {
		if b.Version == v && b.Target == cur.Target {
			return b, nil
		}
	}
	return pg.Build{}, fmt.Errorf("%s=%s: not a PostgreSQL release this program pins for %s", testPGBuildEnv, v, cur.Target)
}

// registerPGBuilds adds the builds this program pins for this node's
// platform (the OS recorded in its platform record) to the cluster's
// table when some are missing. Nothing is written until every member's
// agent knows the command.
func (a *Agent) registerPGBuilds(ctx context.Context) {
	st := a.cn.State()
	osName := platformOS()
	missing := map[string]cluster.PGBuild{}
	for sha, b := range knownPGBuilds() {
		if b.OS != osName || b.Arch != runtime.GOARCH {
			// Only builds for this kind of machine: another kind's
			// entries are registered by its own nodes (and a test's fake
			// OS has none).
			continue
		}
		if _, ok := st.PGBuilds[sha]; !ok {
			missing[sha] = b
		}
	}
	if len(missing) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if !a.pgBuildsOK(cctx, st) {
		return
	}
	if _, err := a.cn.Apply(cctx, &cluster.RegisterPGBuilds{Builds: missing}); err != nil {
		a.log.Info("could not record this program's PostgreSQL builds yet; will retry", "err", err)
		return
	}
	var vs []string
	for _, b := range missing {
		vs = append(vs, b.Version)
	}
	slices.Sort(vs)
	a.log.Info("recorded this program's PostgreSQL builds in the cluster", "versions", strings.Join(slices.Compact(vs), ","))
}

// pgBuildsOK reports whether every member's agent knows RegisterPGBuilds
// (caps "pg_builds"). Once the table exists the answer is yes: every
// member then knew it, and the leader refuses joiners with an older
// protocol (cluster.JoinProtocol 3). Any member that does not answer
// counts as "no": the write waits.
func (a *Agent) pgBuildsOK(ctx context.Context, st cluster.State) bool {
	if len(st.PGBuilds) > 0 {
		return true
	}
	ids := slices.Sorted(maps.Keys(st.Members))
	key := strings.Join(ids, ",")
	a.pgb.mu.Lock()
	if a.pgb.capsKey == key && time.Since(a.pgb.capsAt) < capsCacheFor {
		a.pgb.mu.Unlock()
		return true
	}
	a.pgb.mu.Unlock()
	ask := func(ctx context.Context, id string) (capsResp, error) {
		var r capsResp
		err := a.call(ctx, id, "caps", nil, &r)
		return r, err
	}
	if who, ok := everyMemberKnows(ctx, ids, a.cn.ID(), ask, func(r capsResp) bool { return r.PGBuilds }); !ok {
		a.log.Info("PostgreSQL builds: not every member knows the build table yet; waiting", "member", a.memberName(who))
		return false
	}
	a.pgb.mu.Lock()
	a.pgb.capsKey, a.pgb.capsAt = key, time.Now()
	a.pgb.mu.Unlock()
	return true
}

// everyMemberKnows asks every member but self for its caps and reports
// whether all of them have the capability; otherwise the first that does
// not (an error, e.g. "unknown agent method" from an agent before D37,
// counts as not having it).
func everyMemberKnows(ctx context.Context, ids []string, self string, ask func(context.Context, string) (capsResp, error), has func(capsResp) bool) (string, bool) {
	for _, id := range ids {
		if id == self {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		r, err := ask(cctx, id)
		cancel()
		if err != nil || !has(r) {
			return id, false
		}
	}
	return "", true
}

// localPlatform is this node's platform as it runs now (its record may
// still name the build it ran before an update).
func (a *Agent) localPlatform() cluster.Platform {
	return cluster.Platform{OS: platformOS(), Arch: runtime.GOARCH, PGBuildSHA: a.inst.Build.SHA256}
}

// minorFollowBlocked says why this node must not copy the current main
// node, or "": the main node's recorded build is a newer PostgreSQL minor
// than the build this node runs (D70).
func (a *Agent) minorFollowBlocked(st cluster.State) string {
	p, ok := st.Members[st.PrimaryID]
	if !ok || p.ID == a.cn.ID() || a.inst.Build.SHA256 == "" {
		return ""
	}
	me := a.localPlatform()
	if !st.NewerPG(p.Platform, me) {
		return ""
	}
	mine, _ := st.PGVersionOf(me)
	theirs, _ := st.PGVersionOf(p.Platform)
	return fmt.Sprintf("This computer runs PostgreSQL %s and the main node %s runs PostgreSQL %s. A copy must run the same or a newer PostgreSQL than the main node, so this one waits, its database stopped, until it is updated: update %s on this computer to the version %s runs.",
		mine.Short(), p.Name, theirs.Short(), brand.Command, p.Name)
}

// stopBehindCopy stops this copy's database because it may not copy the
// main node (minorFollowBlocked). Nothing it holds is lost: it is a copy,
// and it resumes from its own data once updated.
func (a *Agent) stopBehindCopy(ctx context.Context, why string) {
	a.setPhase("waiting", why)
	srv, role := a.currentPG()
	if srv == nil || role == "primary" {
		return
	}
	unlock, err := a.lockPG(ctx)
	if err != nil {
		return
	}
	defer unlock()
	a.log.Warn("PostgreSQL builds: the main node runs a newer PostgreSQL; this copy stops until it is updated", "why", why)
	if err := a.stopPG(); err != nil {
		a.log.Warn("stop postgres (older than the main node)", "err", err)
	}
}

// minorSwitchBlocked says why a planned switch must not make target the
// main node, or "": another data node (not the current main node, which
// is updated next in the documented order) runs an older PostgreSQL than
// the target, so it could not copy it (D70).
func minorSwitchBlocked(st cluster.State, target cluster.Member) string {
	var older []string
	for _, m := range st.DataMembers() {
		if m.ID == target.ID || m.ID == st.PrimaryID {
			continue
		}
		if st.NewerPG(target.Platform, m.Platform) {
			older = append(older, m.Name)
		}
	}
	if len(older) == 0 {
		return ""
	}
	v, _ := st.PGVersionOf(target.Platform)
	return fmt.Sprintf("%s runs PostgreSQL %s, newer than %s, which could then not copy it. Update %s first (copies first, then the switch).",
		target.Name, v.Short(), strings.Join(older, ", "), strings.Join(older, ", "))
}

// minorRecordPending says why a switch to target must wait, or "": the
// target runs another build of its PostgreSQL line than its record names
// (it was just updated and has not announced it yet), and the copies would
// compare the old record.
func minorRecordPending(st cluster.State, target cluster.Member, runs cluster.Platform) string {
	if runs.PGBuildSHA == target.Platform.PGBuildSHA || !st.SamePGLine(runs, target.Platform) {
		return ""
	}
	return fmt.Sprintf("%s has not recorded the PostgreSQL it runs since its update yet; try again in a few seconds.", target.Name)
}
