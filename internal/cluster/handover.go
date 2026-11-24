package cluster

import "context"

// TransferLeadership hands Raft leadership to another voter if this node
// leads (nil if it does not). The main-node health check (D58) uses it
// before asking for a planned switch: the switch runs on the leader, and a
// main node whose disk is full or stalled cannot write the Raft log the
// switch needs.
func (n *Node) TransferLeadership(ctx context.Context) error {
	if !n.IsLeader() {
		return nil
	}
	return n.waitFuture(ctx, n.raft.LeadershipTransfer())
}
