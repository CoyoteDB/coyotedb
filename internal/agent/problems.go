package agent

import (
	"cmp"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
)

// Status problems with stable codes (D66). Every problem `coyote status`,
// GET /v1/status and the dashboard show carries a code from problemCodes,
// a short plain line (Summary) for everyone, and a long form (Details,
// Evidence, Commands, Markdown) for people who read logs or hand it to an
// AI agent. The texts live here only: the CLI prints them, the API returns
// them, the dashboard shows what the API returns. Nothing in a problem is
// secret: no invite code, password, key or token is ever put in one.

// Problem is one thing that needs attention, as this node sees it.
type Problem struct {
	Code string `json:"code"`
	// Severity: "problem" (something does not work) or "warning".
	Severity string `json:"severity"`
	// Node: the computer it is about ("": the whole cluster).
	Node string `json:"node,omitempty"`
	// Summary: one plain line.
	Summary string `json:"summary"`
	// Fix: what to do, in one plain sentence.
	Fix string `json:"fix,omitempty"`
	// Details: the long form, paragraphs separated by a blank line: what
	// happened, what it means, likely causes.
	Details string `json:"details"`
	// Evidence: the facts behind it (addresses, times, who saw what).
	Evidence []string `json:"evidence,omitempty"`
	// Commands: exact commands that fix it, in order.
	Commands []string  `json:"commands,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	// Address: the facts of an address problem (address_* codes).
	Address *AddressFacts `json:"address,omitempty"`
	// Markdown: a self-contained block for an AI agent (no secrets).
	Markdown string `json:"markdown"`
}

// AddressFacts are the addresses behind an address problem.
type AddressFacts struct {
	// Recorded: what the cluster records for the node (what the others
	// dial: the previous address when it changed).
	Recorded []string `json:"recorded"`
	// Now: what that computer has now (HOST:PORT), when known.
	Now []string `json:"now,omitempty"`
	// Suggested: what to record instead (`node set-address`).
	Suggested []string `json:"suggested,omitempty"`
	// UnreachableFrom: nodes that cannot connect to it.
	UnreachableFrom []string `json:"unreachable_from,omitempty"`
	// OneWay: it can connect to the others; they cannot connect to it.
	OneWay bool `json:"one_way,omitempty"`
}

// CodeInfo is what a code means in general (`coyote explain CODE`, GET
// /v1/problem-codes). Commands are templates: NODE, HOST, PORT are to be
// filled in.
type CodeInfo struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Meaning  string   `json:"meaning"`
	Causes   []string `json:"causes"`
	Fix      string   `json:"fix"`
	Commands []string `json:"commands"`
	// Where: "status" (a status problem) or "request" (an answer to a
	// command such as set-address).
	Where string `json:"where"`
}

const (
	sevProblem = "problem"
	sevWarning = "warning"
)

// Codes. Stable: clients branch on them.
const (
	codeAddressGone        = "address_gone"
	codeAddressUnreachable = "address_unreachable"
	codeAddressNoAnswer    = "address_no_answer"
	codeAddressNotThisNode = "address_not_this_node"
	codePeersUnreachable   = "peers_unreachable"
	codeWaitingForCluster  = "waiting_for_cluster"
	codeNoLeader           = "no_leader"
	codeNoMain             = "no_main"
	codeMainUnreachable    = "main_unreachable"
	codeNodeUnreachable    = "node_unreachable"
	codeWritesWaiting      = "writes_waiting"
	codeOneCopy            = "one_copy"
	codeWaitingLease       = "waiting_lease"
	codeNoEligibleCopy     = "no_eligible_copy"
	codeTwoNodes           = "two_nodes"
	codeNeedsAttention     = "needs_attention"
	codeNodeWaiting        = "node_waiting"
)

var problemCodes = []CodeInfo{
	{
		Code: codeAddressGone, Severity: sevProblem, Where: "status",
		Title:   "A computer's address changed",
		Meaning: "The nodes find each other at the addresses the cluster records for each of them. This computer no longer has the address the cluster records for it, so the other computers cannot connect to it. CoyoteDB never changes a recorded address by itself: it tells you and waits for you to confirm the new one. While it lasts, a copy on that computer stops receiving changes and cannot take over; if it was the main node, it stops being the main node within seconds (it cannot prove the others reach it) and, where failover is automatic, another computer takes over. No saved write is lost.",
		Causes: []string{
			"The router gave the computer a new address (DHCP), for example after a restart of the router or the computer.",
			"The computer moved to another network (Wi-Fi to cable, another office, a hotspot).",
			"A VPN or Tailscale was turned off, or the computer left the tailnet.",
			"A container or virtual machine was recreated and got a new address.",
		},
		Fix:      "Record the computer's new address. The command checks first that the node answers there, from the computer you run it on. To stop it happening again, reserve the address in your router (a DHCP reservation) or use Tailscale addresses.",
		Commands: []string{brand.Command + " node set-address NODE HOST:PORT"},
	},
	{
		Code: codeAddressUnreachable, Severity: sevProblem, Where: "status",
		Title:   "The other computers cannot connect to a computer",
		Meaning: "This computer can connect to the others, but they cannot connect to it at the address the cluster records (one-way). Nodes need both directions: the leader sends its updates to every node, and copies connect to the main node to follow it. A main node that no leader reaches gives up its lease, so another can take over safely.",
		Causes: []string{
			"A firewall on this computer (or the network) blocks incoming connections on the cluster port.",
			"The recorded address is one this computer cannot be reached at: a router's address without port forwarding, a container's private address, a wrong --advertise.",
			"The computer's address changed to one behind a router (NAT) that the others cannot reach.",
		},
		Fix:      "Allow incoming connections on the cluster port (7481 by default) on that computer, or record an address the others can reach it at.",
		Commands: []string{brand.Command + " node set-address NODE HOST:PORT"},
	},
	{
		Code: codeAddressNoAnswer, Severity: sevProblem, Where: "request",
		Title:    "Nothing answered at the address",
		Meaning:  "`node set-address` connects to each address first, from the computer you run it on, before it records anything. None of the addresses accepted a connection, so nothing was changed.",
		Causes:   []string{"A typo in the address or the port.", "The node is not running.", "A firewall blocks the port.", "This computer cannot reach that network (only the others can)."},
		Fix:      "Check the address and that the node runs. If the node is down, or only other computers can reach it, record it anyway with --force.",
		Commands: []string{brand.Command + " node set-address NODE HOST:PORT", brand.Command + " node set-address --force NODE HOST:PORT"},
	},
	{
		Code: codeAddressNotThisNode, Severity: sevProblem, Where: "request",
		Title:    "Another program or node answered at the address",
		Meaning:  "Something accepted the connection, but it did not prove it is that node of this cluster (its certificate is another node's, or not this cluster's). Nothing was changed: recording it would send that node's traffic to the wrong computer.",
		Causes:   []string{"The address belongs to another computer (a typo, or the address was handed to another device).", "It is another node of this cluster: check the name.", "Another program listens on that port."},
		Fix:      "Check which computer has that address now, and use the address of the computer that runs the node.",
		Commands: []string{brand.Command + " node set-address NODE HOST:PORT"},
	},
	{
		Code: codePeersUnreachable, Severity: sevProblem, Where: "status",
		Title:    "No other computer answers",
		Meaning:  "This node cannot connect to any other node at the addresses it knows. Without a majority that reaches each other there is no leader, and nothing can change.",
		Causes:   []string{"The other computers are off or the program is not running on them.", "Every computer got a new address while the cluster was off (a router restart can do that).", "This computer is on another network than the others."},
		Fix:      "Start the other computers. If their addresses changed, tell each computer where the others are now with --local (it needs no leader); once a majority reach each other the cluster runs again, and each node then shows the address to record for it.",
		Commands: []string{brand.Command + " node set-address --local NODE HOST:PORT"},
	},
	{
		Code: codeWaitingForCluster, Severity: sevWarning, Where: "status",
		Title:    "Waiting to reach the other nodes",
		Meaning:  "This node has not heard from the cluster since it started, so it does not know the cluster's state yet. It keeps trying the addresses it saved.",
		Causes:   []string{"The other computers are still starting.", "Not enough of them run to choose a leader.", "Their addresses changed."},
		Fix:      "Wait a little, and start the other computers. If nobody answers for long, see peers_unreachable.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeNoLeader, Severity: sevProblem, Where: "status",
		Title:    "The cluster has no leader",
		Meaning:  "Not enough voting nodes reach each other to choose a leader. Nothing can change until they do: no failover, no new settings, no new addresses (except --local). Also the answer to a request that needs a leader. The dashboard's failover_code for this is no_quorum.",
		Causes:   []string{"More than half of the voting nodes are down or cut off.", "Addresses changed and the nodes cannot find each other."},
		Fix:      "Start the stopped computers or reconnect them. If addresses changed, use `node set-address --local` on each computer.",
		Commands: []string{brand.Command + " status --details", brand.Command + " node set-address --local NODE HOST:PORT"},
	},
	{
		Code: codeNoMain, Severity: sevProblem, Where: "status",
		Title:    "No main node",
		Meaning:  "No node is the main node right now, so applications cannot write. During a failover this lasts seconds.",
		Causes:   []string{"A failover is in progress.", "No copy is safe to promote (see no_eligible_copy)."},
		Fix:      "Wait for the failover; if it does not come, look at the other problems listed.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeMainUnreachable, Severity: sevWarning, Where: "status",
		Title:    "The main node does not answer this node",
		Meaning:  "This node cannot get a report from the main node. Others may still reach it.",
		Causes:   []string{"The main node is down or restarting.", "The network between the two is cut.", "The main node's address changed (see address_gone)."},
		Fix:      "Check the main node's computer. If it stays gone, the cluster fails over by itself where it can.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeNodeUnreachable, Severity: sevWarning, Where: "status",
		Title:    "A node does not answer",
		Meaning:  "This node cannot get a report from that node.",
		Causes:   []string{"The computer is off, asleep, or the program stopped.", "The network is cut.", "Its address changed (it reports address_gone when it can still connect out)."},
		Fix:      "Check that computer and start the program there.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeWritesWaiting, Severity: sevProblem, Where: "status",
		Title:    "Writes are waiting for a copy",
		Meaning:  "Each write must reach a copy before it is confirmed, so that no confirmed write can be lost, and not enough copies are connected. Writes hang until a copy is back or, in the default mode, until the main node carries on alone after the wait set in the settings. The dashboard's failover_code is strict_waiting when the cluster is set to wait for good.",
		Causes:   []string{"The copy's computer is down or restarting.", "The copy cannot connect to the main node (network, address, firewall)."},
		Fix:      "Bring a copy back. Do not change the safety setting to make writes go on unless you accept running on one copy.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeOneCopy, Severity: sevWarning, Where: "status",
		Title:    "Running on one copy",
		Meaning:  "Writes are saved on the main node only: if its disk dies now, writes since then are lost. Another node cannot take over until a copy has caught up again.",
		Causes:   []string{"A copy was gone longer than the wait set in the settings, and the main node carried on alone."},
		Fix:      "Bring the copy back; it catches up by itself and protection returns.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeWaitingLease, Severity: sevWarning, Where: "status",
		Title:    "The main node stopped renewing its lease",
		Meaning:  "The main node has not renewed its lease. Before another node takes over, the cluster waits until the old main node's lease has run out for sure, so two main nodes never run at once.",
		Causes:   []string{"The main node is down or cut off.", "No leader reaches it any more (its address changed, or incoming connections are blocked)."},
		Fix:      "Nothing: another node takes over within the lease time. Then bring the old main node's computer back; it returns as a copy.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeNoEligibleCopy, Severity: sevProblem, Where: "status",
		Title:    "No copy is safe to take over",
		Meaning:  "The main node is gone, and no copy is known to have every confirmed write, so none is promoted (that could lose confirmed writes).",
		Causes:   []string{"The cluster was running on one copy when the main node died.", "The copies were behind."},
		Fix:      "Bring the old main node back; it takes over again with all its data.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeTwoNodes, Severity: sevProblem, Where: "status",
		Title:    "The main node is gone and nobody takes over by itself",
		Meaning:  "With two data nodes and no witness, the remaining node cannot tell a dead main node from a cut network, so it does not take over by itself.",
		Causes:   []string{"The main node's computer is down or cut off, in a cluster of two data nodes without a witness."},
		Fix:      "Bring the main node back. Add a witness (a third, small computer that only votes) to make failover automatic.",
		Commands: []string{brand.Command + " invite", brand.Command + " witness"},
	},
	{
		Code: codeNeedsAttention, Severity: sevProblem, Where: "status",
		Title:    "A node needs attention",
		Meaning:  "A node stopped doing its job and cannot fix it by itself; its message says why.",
		Causes:   []string{"See the node's message: a full disk, damaged data, a failed step."},
		Fix:      "Follow the node's message; its log has the details.",
		Commands: []string{brand.Command + " status --details"},
	},
	{
		Code: codeNodeWaiting, Severity: sevWarning, Where: "status",
		Title:    "A node is waiting",
		Meaning:  "A node is waiting for something before it goes on; its message says what.",
		Causes:   []string{"See the node's message."},
		Fix:      "Usually nothing: it goes on by itself. If it lasts, follow the message.",
		Commands: []string{brand.Command + " status --details"},
	},
}

// ProblemCodes lists every code (a copy).
func ProblemCodes() []CodeInfo { return slices.Clone(problemCodes) }

// LookupCode finds a code's description.
func LookupCode(code string) (CodeInfo, bool) {
	i := slices.IndexFunc(problemCodes, func(c CodeInfo) bool { return c.Code == code })
	if i < 0 {
		return CodeInfo{}, false
	}
	return problemCodes[i], true
}

// ExplainText is `coyote explain CODE`.
func ExplainText(c CodeInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n\n%s\n\nLikely causes:\n", c.Code, c.Title, c.Meaning)
	for _, x := range c.Causes {
		fmt.Fprintf(&b, "  - %s\n", x)
	}
	fmt.Fprintf(&b, "\nFix: %s\n", c.Fix)
	if len(c.Commands) > 0 {
		b.WriteString("\n")
		for _, x := range c.Commands {
			fmt.Fprintf(&b, "  %s\n", x)
		}
	}
	return b.String()
}

// newProblem starts a problem with the code's severity.
func newProblem(code, node, summary string) Problem {
	c, _ := LookupCode(code)
	return Problem{Code: code, Severity: cmp.Or(c.Severity, sevWarning), Node: node, Summary: summary}
}

// finish fills Details (what happened, then the code's meaning and
// causes) and Markdown. viewer is the node that made the status.
func (p *Problem) finish(what, viewer string, at time.Time) {
	c, _ := LookupCode(p.Code)
	var parts []string
	if what != "" {
		parts = append(parts, what)
	}
	if c.Meaning != "" {
		parts = append(parts, c.Meaning)
	}
	if len(c.Causes) > 0 {
		parts = append(parts, "Likely causes: "+strings.Join(c.Causes, " "))
	}
	if p.Fix == "" {
		p.Fix = c.Fix
	}
	p.Details = strings.Join(parts, "\n\n")
	p.Markdown = p.markdown(c, viewer, at)
}

func (p *Problem) markdown(c CodeInfo, viewer string, at time.Time) string {
	var b strings.Builder
	about := ""
	if p.Node != "" {
		about = " on " + p.Node
	}
	fmt.Fprintf(&b, "### CoyoteDB %s `%s`%s\n\n", p.Severity, p.Code, about)
	fmt.Fprintf(&b, "**Summary:** %s\n\n", p.Summary)
	if c.Title != "" {
		fmt.Fprintf(&b, "**What `%s` means (%s):** %s\n\n", p.Code, c.Title, c.Meaning)
	}
	if len(c.Causes) > 0 {
		b.WriteString("**Likely causes:**\n\n")
		for _, x := range c.Causes {
			fmt.Fprintf(&b, "- %s\n", x)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "**Evidence** (status from node %s at %s):\n\n", viewer, at.UTC().Format(time.RFC3339))
	if !p.Since.IsZero() {
		fmt.Fprintf(&b, "- Since: %s\n", p.Since.UTC().Format(time.RFC3339))
	}
	for _, x := range p.Evidence {
		fmt.Fprintf(&b, "- %s\n", x)
	}
	b.WriteString("\n")
	if p.Fix != "" {
		fmt.Fprintf(&b, "**Fix:** %s\n\n", p.Fix)
	}
	if len(p.Commands) > 0 {
		b.WriteString("```sh\n")
		for _, x := range p.Commands {
			b.WriteString(x + "\n")
		}
		b.WriteString("```\n\n")
	}
	fmt.Fprintf(&b, "More: `%s explain %s`, `%s status --details`, `%s status --json`. This text never includes invite codes, passwords or keys.\n",
		brand.Command, p.Code, brand.Command, brand.Command)
	return b.String()
}

// hostsOf drops the ports, for plain sentences ("192.168.1.20").
func hostsOf(addrs []string) string {
	var out []string
	for _, a := range addrs {
		h, _, err := net.SplitHostPort(a)
		if err != nil {
			h = a
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return strings.Join(out, ", ")
}

func timeText(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format("2006-01-02 15:04:05 UTC"), now.Sub(t).Round(time.Second))
}

// addressProblem builds address_gone / address_unreachable for node
// (named name) from its own report r. viewer: the node making the status;
// self: r is this node's own; reachable: viewer gets node's reports;
// leader: viewer knows a leader.
func addressProblem(name string, r addrReport, viewer string, self, reachable, leader bool, received, now time.Time) Problem {
	if len(r.Gone) > 0 {
		return addressGone(name, r, viewer, self, reachable, leader, received, now)
	}
	return addressUnreachable(name, r, viewer, self, received, now)
}

func addressGone(name string, r addrReport, viewer string, self, reachable, leader bool, received, now time.Time) Problem {
	still := slices.DeleteFunc(slices.Clone(r.Recorded), func(x string) bool { return slices.Contains(r.Gone, x) })
	var summary string
	switch {
	case len(r.Now) > 0 && len(still) == 0:
		was, is := hostsOf(r.Gone), hostsOf(r.Now)
		rec := hostsOf(r.Recorded)
		if was == is { // only the port differs: show it
			was, is, rec = strings.Join(r.Gone, ", "), strings.Join(r.Now, ", "), strings.Join(r.Recorded, ", ")
		}
		summary = fmt.Sprintf("%s's address changed: it was %s, it is now %s. The cluster still records %s, so the other computers cannot reach it.",
			name, was, is, rec)
	case len(still) == 0:
		summary = fmt.Sprintf("%s no longer has the address %s, and no new address was found on it. The cluster still records %s, so the other computers cannot reach it.",
			name, hostsOf(r.Gone), hostsOf(r.Recorded))
	default:
		summary = fmt.Sprintf("%s no longer has %s, one of the addresses the cluster records for it; the others can still try %s.",
			name, hostsOf(r.Gone), hostsOf(still))
	}
	p := newProblem(codeAddressGone, name, summary)
	if len(still) > 0 {
		p.Severity = sevWarning
	}
	p.Since = r.Since
	suggested := slices.Clone(r.Now)
	if len(suggested) == 0 {
		suggested = still
	}
	p.Address = &AddressFacts{Recorded: slices.Clone(r.Recorded), Now: slices.Clone(r.Now), Suggested: suggested}
	target := "NEWHOST:PORT"
	if len(suggested) > 0 {
		target = strings.Join(suggested, ",")
	}
	p.Commands = []string{fmt.Sprintf("%s node set-address %s %s", brand.Command, name, target)}
	p.Fix = fmt.Sprintf("Record %s's new address, from any computer of the cluster (it checks first that %s answers there).", name, name)
	if len(suggested) == 0 {
		p.Fix = fmt.Sprintf("Find %s's new address (on that computer, or in your router), then record it from any computer of the cluster.", name)
	}
	if !leader {
		p.Commands = append(p.Commands, fmt.Sprintf("%s node set-address --local %s %s", brand.Command, name, target))
		p.Fix += " The cluster has no leader right now, so first run the --local command on each other computer (it needs no leader); once they reach each other, run the first command."
	}
	p.Evidence = append(p.Evidence,
		fmt.Sprintf("The cluster records %s at %s (the previous address).", name, strings.Join(r.Recorded, ", ")),
		fmt.Sprintf("%s's computer no longer has %s.", name, strings.Join(r.Gone, ", ")))
	if len(r.Now) > 0 {
		p.Evidence = append(p.Evidence, fmt.Sprintf("%s's computer has now: %s (what it would choose at a first start).", name, strings.Join(r.Now, ", ")))
	}
	p.Evidence = append(p.Evidence, fmt.Sprintf("Noticed by %s itself at %s.", name, timeText(r.Since, now)))
	if !self {
		p.Evidence = append(p.Evidence, fmt.Sprintf("%s told %s at %s (it can still connect out).", name, viewer, timeText(received, now)))
		if !reachable {
			p.Evidence = append(p.Evidence, fmt.Sprintf("%s cannot get a report from %s at its recorded address.", viewer, name))
			p.Address.UnreachableFrom = []string{viewer}
		}
	}
	what := fmt.Sprintf("%s's computer had %s, the address the cluster records for it, and does not have it any more", name, hostsOf(r.Gone))
	if len(r.Now) > 0 {
		what += fmt.Sprintf("; it has %s now", hostsOf(r.Now))
	}
	what += ". The other computers still dial the old address."
	p.finish(what, viewer, now)
	return p
}

func addressUnreachable(name string, r addrReport, viewer string, self bool, received, now time.Time) Problem {
	contact := "never since it started"
	if r.LeaderContactMS >= 0 {
		contact = (time.Duration(r.LeaderContactMS) * time.Millisecond).Round(time.Second).String() + " ago"
	}
	var summary string
	if self {
		summary = fmt.Sprintf("The other computers cannot connect to %s at %s, although %s can connect to them (one-way), since %s.",
			name, hostsOf(r.Recorded), name, r.Since.Local().Format("15:04:05"))
	} else {
		summary = fmt.Sprintf("%s cannot connect to %s at %s, although %s still connects to the others (one-way), since %s.",
			viewer, name, hostsOf(r.Recorded), name, r.Since.Local().Format("15:04:05"))
	}
	p := newProblem(codeAddressUnreachable, name, summary)
	p.Since = r.Since
	p.Address = &AddressFacts{Recorded: slices.Clone(r.Recorded), OneWay: true}
	if !self {
		p.Address.UnreachableFrom = []string{viewer}
	}
	p.Fix = fmt.Sprintf("On %s, allow incoming connections on the cluster port (%s); if %s is the wrong address for it, record the right one.", name, portsOf(r.Recorded), hostsOf(r.Recorded))
	p.Commands = []string{fmt.Sprintf("%s node set-address %s HOST:%s", brand.Command, name, firstPort(r.Recorded))}
	p.Evidence = append(p.Evidence,
		fmt.Sprintf("The cluster records %s at %s.", name, strings.Join(r.Recorded, ", ")),
		fmt.Sprintf("%s's computer has that address; no leader has reached it for %s, while it reaches other nodes.", name, contact),
		fmt.Sprintf("Noticed by %s itself at %s.", name, timeText(r.Since, now)))
	if !self {
		p.Evidence = append(p.Evidence,
			fmt.Sprintf("%s told %s at %s, over a connection it opened itself.", name, viewer, timeText(received, now)),
			fmt.Sprintf("%s cannot get a report from %s at %s.", viewer, name, strings.Join(r.Recorded, ", ")))
	}
	p.finish(fmt.Sprintf("%s connects out to the other nodes, but none of them can connect in to it at %s.", name, hostsOf(r.Recorded)), viewer, now)
	return p
}

func portsOf(addrs []string) string {
	var out []string
	for _, a := range addrs {
		if _, p, err := net.SplitHostPort(a); err == nil && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "7481 by default"
	}
	return strings.Join(out, ", ")
}

// summaries is the plain lines of ps (Status.Warnings keeps them, for
// older clients).
func summaries(ps []Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Summary)
	}
	return out
}
