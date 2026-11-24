package pg

import (
	"context"
	"errors"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Main-node health (D58). The main node's agent checks, about once a
// second, that its Postgres can still make a write durable on this
// machine's disk, independent of the copies: a disk that stalls or a
// Postgres that hangs otherwise leaves a main node that renews its lease
// and acknowledges nothing (worst-case finding D2).

// HealthProbeSQL writes one tiny WAL record and flushes it to disk:
// a non-transactional logical message with flush = true (PostgreSQL 17+).
// It touches no table, takes no xid and no lock any client can hold, and
// does not wait for the copies (no transaction commits), so it measures
// exactly "this machine's database can still write and fsync its WAL".
// Copies replay the record as a no-op.
const HealthProbeSQL = `SELECT pg_logical_emit_message(false, 'coyote_health', '', true)`

// ProbeWrite runs the health probe on c. A nil error means a WAL record
// reached this node's disk while ctx lasted.
func ProbeWrite(ctx context.Context, c *pgx.Conn) error {
	_, err := c.Exec(ctx, HealthProbeSQL)
	return err
}

// ProbeOutcome sorts a probe's result for the health check.
type ProbeOutcome int

const (
	// ProbeOK: the write reached the disk.
	ProbeOK ProbeOutcome = iota
	// ProbeStalled: no answer in time (a hung disk or a frozen Postgres).
	ProbeStalled
	// ProbeDiskFull: the disk (or quota) is full.
	ProbeDiskFull
	// ProbeOther: an answer that says nothing about this machine's disk
	// (Postgres down or restarting, refused, a setting): not counted.
	ProbeOther
)

// ClassifyProbe sorts err from a probe (or the connection for it) whose
// context was ctx.
func ClassifyProbe(ctx context.Context, err error) ProbeOutcome {
	switch {
	case err == nil:
		return ProbeOK
	case DiskFullError(err):
		return ProbeDiskFull
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || pgconn.Timeout(err):
		return ProbeStalled
	}
	return ProbeOther
}

// DiskFullError: err says the disk (or the user's quota) is full,
// whether it came from this process or from Postgres (SQLSTATE 53100
// disk_full, or its message).
func DiskFullError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "53100" {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left on device") || strings.Contains(s, "disk quota exceeded") ||
		strings.Contains(s, "there is not enough space on the disk")
}
