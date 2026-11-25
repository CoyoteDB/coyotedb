// Command probe is the chaos checker's commit probe (docs/TESTING.md "Two
// main nodes: the commit probe"), run inside a node's container by
// test/docker (TestDockerUpdate). PostgreSQL listens on the container's
// 127.0.0.1 only (rule 11), so the probe has to run there.
//
// Every 10 ms it commits, on one long-lived connection to the node's own
// Postgres with synchronous_commit = local,
//
//	SELECT txid_current(), pg_postmaster_start_time(), pg_backend_pid()
//
// and prints one JSON line per probe: when it was sent and answered, read
// from internal/mono (CLOCK_BOOTTIME). Every container on one Docker host
// shares that host's (or Docker Desktop's VM's) kernel and so one clock,
// which is what check.TwoMains needs to compare nodes. It stops when its
// standard input closes.
//
//	COYOTE_PROBE_CONN  the connection string (coyote conninfo --admin)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"coyotedb.com/coyote/internal/mono"
)

// Line is one probe; the kinds are check.Kind's names.
type Line struct {
	Start  int64  `json:"start_ns"`
	End    int64  `json:"end_ns"`
	Kind   string `json:"kind"`
	Conn   string `json:"conn,omitempty"`
	Detail string `json:"detail,omitempty"`
}

const (
	every   = 10 * time.Millisecond
	timeout = 3 * time.Second
)

func main() {
	cs := os.Getenv("COYOTE_PROBE_CONN")
	if cs == "" {
		fmt.Fprintln(os.Stderr, "probe: COYOTE_PROBE_CONN is not set")
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	enc := json.NewEncoder(os.Stdout)
	emit := func(l Line) {
		if err := enc.Encode(l); err != nil {
			os.Exit(1)
		}
	}
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			conn.Close(context.Background())
		}
	}()
	for ctx.Err() == nil {
		if conn == nil {
			cctx, ccancel := context.WithTimeout(ctx, timeout)
			l := Line{Start: int64(mono.Now())}
			c, err := pgx.Connect(cctx, cs)
			if err == nil {
				if _, err = c.Exec(cctx, "SET synchronous_commit = local"); err != nil {
					c.Close(context.Background())
				}
			}
			ccancel()
			l.End = int64(mono.Now())
			if err != nil {
				l.Kind, l.Detail = kind(err), err.Error()
				emit(l)
				time.Sleep(every) // pacing between probes
				continue
			}
			conn = c
		}
		qctx, qcancel := context.WithTimeout(ctx, timeout)
		l := Line{Start: int64(mono.Now())}
		var xid, started string
		var pid int
		err := conn.QueryRow(qctx, `SELECT txid_current()::text, pg_postmaster_start_time()::text, pg_backend_pid()`).Scan(&xid, &started, &pid)
		l.End = int64(mono.Now())
		qcancel()
		if err == nil {
			l.Kind, l.Conn = "rw", fmt.Sprintf("%s/%d", started, pid)
		} else {
			l.Kind, l.Detail = kind(err), err.Error()
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Severity != "ERROR" {
				conn.Close(context.Background())
				conn = nil
			}
		}
		if ctx.Err() != nil {
			return
		}
		emit(l)
		time.Sleep(every) // pacing between probes
	}
}

// kind is what a failed probe or connection attempt proves (the same
// rules as test/chaos probeKind).
func kind(err error) string {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "down"
	case errors.As(err, &pe) && pe.Code == "25006":
		return "not-rw" // cannot execute txid_current() during recovery
	case errors.As(err, &pe) && pe.Code == "57P03" && !strings.Contains(pe.Message, "shutting down"):
		return "not-rw" // starting up / not yet accepting / in recovery mode
	}
	return "unknown"
}
