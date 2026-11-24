package pg

import (
	"context"
	"errors"
	"fmt"
)

// WALReceiver is what a standby's WAL receiver is doing right now.
type WALReceiver struct {
	// PID of the WAL receiver process; 0 when none runs (the startup
	// process starts one again every wal_retrieve_retry_interval).
	PID int
	// Streaming: pg_stat_wal_receiver reports status 'streaming'. While a
	// receiver is still opening its connection it has no row there yet.
	Streaming bool
	// WaitEvent is the receiver's wait event (LibpqwalreceiverConnect while
	// it opens its connection), "" if none.
	WaitEvent string
}

// WALReceiverState reads this standby's WAL receiver from its own Postgres.
func (n Node) WALReceiverState(ctx context.Context) (WALReceiver, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return WALReceiver{}, err
	}
	defer c.Close(context.Background())
	var r WALReceiver
	var pid *int
	var wait *string
	err = c.QueryRow(ctx, `SELECT
		(SELECT pid FROM pg_stat_activity WHERE backend_type = 'walreceiver' LIMIT 1),
		(SELECT wait_event FROM pg_stat_activity WHERE backend_type = 'walreceiver' LIMIT 1),
		EXISTS (SELECT 1 FROM pg_stat_wal_receiver WHERE status = 'streaming')`).Scan(&pid, &wait, &r.Streaming)
	if err != nil {
		return WALReceiver{}, err
	}
	if pid != nil {
		r.PID = *pid
	}
	if wait != nil {
		r.WaitEvent = *wait
	}
	return r, nil
}

// errNotReceiver: the pid is no longer this node's WAL receiver.
var errNotReceiver = errors.New("that process is no longer the WAL receiver")

// RestartWALReceiver ends the WAL receiver pid (asking it to stop, as
// Postgres does itself when primary_conninfo changes); the startup process
// starts a fresh one, with a fresh connection, within
// wal_retrieve_retry_interval. It is for a receiver stuck on a connection
// whose other end is gone (D57). The pid is checked again right before the
// signal, so a receiver that ended on its own in between is left alone.
// Postgres refuses pg_terminate_backend for it (not a backend), so the
// signal is sent directly (Unix) or through pg_ctl kill (Windows).
func (n Node) RestartWALReceiver(ctx context.Context, inst Install, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("restart WAL receiver: bad pid %d", pid)
	}
	cur, err := n.WALReceiverState(ctx)
	if err != nil {
		return fmt.Errorf("restart WAL receiver: %w", err)
	}
	if cur.PID != pid {
		return errNotReceiver
	}
	if err := terminatePostgresChild(inst, pid); err != nil {
		return fmt.Errorf("restart WAL receiver (pid %d): %w", pid, err)
	}
	return nil
}

// IsNotReceiver reports whether RestartWALReceiver did nothing because the
// receiver had already been replaced.
func IsNotReceiver(err error) bool { return errors.Is(err, errNotReceiver) }
