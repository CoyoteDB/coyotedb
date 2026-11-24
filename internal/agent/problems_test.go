package agent

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Every code a status or a request can carry is in the catalogue, once,
// with every text filled in (`coyote explain`, /v1/problem-codes).
func TestProblemCodesComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range ProblemCodes() {
		if seen[c.Code] {
			t.Fatalf("code %s listed twice", c.Code)
		}
		seen[c.Code] = true
		if c.Title == "" || c.Meaning == "" || len(c.Causes) == 0 || c.Fix == "" || (c.Severity != sevProblem && c.Severity != sevWarning) || (c.Where != "status" && c.Where != "request") {
			t.Fatalf("code %s is incomplete: %+v", c.Code, c)
		}
		if !strings.Contains(ExplainText(c), c.Meaning) {
			t.Fatalf("explain %s misses the meaning", c.Code)
		}
	}
	for _, c := range []string{codeAddressGone, codeAddressUnreachable, codeAddressNoAnswer, codeAddressNotThisNode, codePeersUnreachable,
		codeWaitingForCluster, codeNoLeader, codeNoMain, codeMainUnreachable, codeNodeUnreachable, codeWritesWaiting, codeOneCopy,
		codeWaitingLease, codeNoEligibleCopy, codeTwoNodes, codeNeedsAttention, codeNodeWaiting} {
		if !seen[c] {
			t.Fatalf("code %s has no description", c)
		}
	}
}

// address_gone names the previous and the new address, the cause, and the
// exact command; the others' view adds who cannot reach it.
func TestAddressGoneProblem(t *testing.T) {
	now := time.Date(2026, 11, 24, 14, 3, 10, 0, time.UTC)
	r := addrReport{Recorded: []string{"192.168.1.20:7481"}, Gone: []string{"192.168.1.20:7481"}, Now: []string{"192.168.1.35:7481"}, Since: now.Add(-time.Minute)}
	p := addressProblem("node2", r, "node1", false, false, true, now.Add(-2*time.Second), now)
	want := "node2's address changed: it was 192.168.1.20, it is now 192.168.1.35. The cluster still records 192.168.1.20, so the other computers cannot reach it."
	if p.Code != codeAddressGone || p.Severity != sevProblem || p.Node != "node2" || p.Summary != want {
		t.Fatalf("problem: %+v", p)
	}
	if !slices.Equal(p.Commands, []string{"coyote node set-address node2 192.168.1.35:7481"}) {
		t.Fatalf("commands: %q", p.Commands)
	}
	if p.Address == nil || !slices.Equal(p.Address.Suggested, []string{"192.168.1.35:7481"}) || !slices.Equal(p.Address.UnreachableFrom, []string{"node1"}) {
		t.Fatalf("address facts: %+v", p.Address)
	}
	for _, s := range []string{"192.168.1.20:7481", "192.168.1.35:7481", "node1 cannot get a report", "2026-11-24 14:02:10 UTC"} {
		if !strings.Contains(strings.Join(p.Evidence, "\n"), s) {
			t.Fatalf("evidence misses %q: %q", s, p.Evidence)
		}
	}
	for _, s := range []string{"`address_gone`", "coyote node set-address node2 192.168.1.35:7481", "never includes invite codes"} {
		if !strings.Contains(p.Markdown, s) {
			t.Fatalf("markdown misses %q:\n%s", s, p.Markdown)
		}
	}
	if !strings.Contains(p.Details, "DHCP") {
		t.Fatalf("details give no cause: %s", p.Details)
	}
	// No leader known: the --local command comes too.
	p = addressProblem("node2", r, "node2", true, true, false, now, now)
	if len(p.Commands) != 2 || !strings.Contains(p.Commands[1], "--local node2 192.168.1.35:7481") {
		t.Fatalf("no-leader commands: %q", p.Commands)
	}
	// Nothing new found: a placeholder, never a made-up address.
	r.Now = nil
	p = addressProblem("node2", r, "node2", true, true, true, now, now)
	if !strings.Contains(p.Commands[0], "NEWHOST:PORT") || strings.Contains(p.Summary, "it is now") {
		t.Fatalf("unknown new address: %+v", p)
	}
}

func TestAddressUnreachableProblem(t *testing.T) {
	now := time.Now()
	r := addrReport{Recorded: []string{"10.0.0.5:7481"}, NoLeader: true, LeaderContactMS: 42000, Since: now.Add(-30 * time.Second)}
	p := addressProblem("n3", r, "n1", false, false, true, now, now)
	if p.Code != codeAddressUnreachable || !p.Address.OneWay || !strings.Contains(p.Summary, "one-way") || !strings.Contains(p.Summary, "n1 cannot connect to n3 at 10.0.0.5") {
		t.Fatalf("problem: %+v", p)
	}
	if !strings.Contains(strings.Join(p.Evidence, " "), "42s") {
		t.Fatalf("evidence: %q", p.Evidence)
	}
}

// The status problems carry codes; warnings keep the same sentences.
func TestStatusProblemsHaveCodes(t *testing.T) {
	st, s := threeNodes()
	s.Nodes[2].Reachable, s.Nodes[2].Report = false, nil
	ps, _ := statusProblems(st, s, s.Nodes[0].Report, "n2", time.Now())
	var codes []string
	for _, p := range ps {
		codes = append(codes, p.Code)
		if p.Summary == "" || p.Details == "" || p.Markdown == "" {
			t.Fatalf("incomplete %+v", p)
		}
	}
	if !slices.Contains(codes, codeWritesWaiting) || !slices.Contains(codes, codeNodeUnreachable) {
		t.Fatalf("codes: %q", codes)
	}
	// A node with an address problem is not also "not reachable".
	ps = append(ps, Problem{Code: codeAddressGone, Node: "n3"})
	for _, p := range dropCoveredByAddress(ps) {
		if p.Code == codeNodeUnreachable && p.Node == "n3" {
			t.Fatal("n3 listed as unreachable next to its address problem")
		}
	}
}

// A report from another node expires unless repeated; an empty one clears.
func TestAddrWatchReports(t *testing.T) {
	var w addrWatch
	t0 := time.Now()
	w.receive("n2", addrReport{Recorded: []string{"a:1"}, Gone: []string{"a:1"}}, t0)
	if len(w.received(t0.Add(time.Second))) != 1 {
		t.Fatal("report not kept")
	}
	if len(w.received(t0.Add(addrReportTTL+time.Second))) != 0 {
		t.Fatal("report not expired")
	}
	w.receive("n2", addrReport{Recorded: []string{"a:1"}, Gone: []string{"a:1"}}, t0)
	w.receive("n2", addrReport{}, t0)
	if len(w.received(t0)) != 0 {
		t.Fatal("empty report did not clear")
	}
}

func TestRefusedCode(t *testing.T) {
	if refusedCode([]PeerAddrProbe{{Code: codeAddressNoAnswer}}) != codeAddressNoAnswer {
		t.Fatal("no answer")
	}
	if refusedCode([]PeerAddrProbe{{Code: codeAddressNoAnswer}, {Code: codeAddressNotThisNode}}) != codeAddressNotThisNode {
		t.Fatal("wrong node")
	}
}
