package agent

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func TestRecvWatchRestartsAStuckReceiver(t *testing.T) {
	var w recvWatch
	t0 := time.Now()
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	// One receiver process stuck connecting: restarted after 30 s, and
	// not again for another 30 s.
	for s := 0; s < 30; s++ {
		if v := w.observe(at(s), recvSample{pid: 100}); v.restart || v.warn {
			t.Fatalf("second %d: %+v too early", s, v)
		}
	}
	if v := w.observe(at(30), recvSample{pid: 100}); !v.restart {
		t.Fatal("stuck receiver not restarted after 30 s")
	}
	if v := w.observe(at(31), recvSample{pid: 100}); v.restart {
		t.Fatal("restarted twice in a row")
	}
	// The restart gave a new process; it is judged on its own.
	if v := w.observe(at(36), recvSample{pid: 101}); v.restart {
		t.Fatal("new receiver restarted at once")
	}
	// Not streaming for a minute in all: said once.
	v := w.observe(at(60), recvSample{pid: 101})
	if !v.warn {
		t.Fatal("no warning after a minute without streaming")
	}
	if v := w.observe(at(61), recvSample{pid: 101}); v.warn {
		t.Fatal("warned twice")
	}
	if v := w.observe(at(66), recvSample{pid: 101}); !v.restart {
		t.Fatal("second stuck receiver not restarted")
	}
	// Streaming again ends the episode; a new one starts from scratch.
	w.observe(at(70), recvSample{streaming: true, pid: 101})
	if v := w.observe(at(71), recvSample{pid: 102}); v.restart || v.warn {
		t.Fatalf("new episode acted at once: %+v", v)
	}
	if v := w.observe(at(131), recvSample{pid: 102}); !v.restart || !v.warn {
		t.Fatalf("new episode: %+v, want restart and warn", v)
	}
}

func TestRecvWatchLeavesFailingReceiversAlone(t *testing.T) {
	// A main node that cannot be reached: each receiver fails by itself
	// and a new one starts (new pid, or none in between). Nothing to
	// restart; the person is still told after a minute.
	var w recvWatch
	t0 := time.Now()
	warned := false
	for s := 0; s <= 120; s++ {
		pid := 0
		if s%6 != 0 {
			pid = 1000 + s/6
		}
		v := w.observe(t0.Add(time.Duration(s)*time.Second), recvSample{pid: pid})
		if v.restart {
			t.Fatalf("second %d: restarted a receiver that is not stuck", s)
		}
		warned = warned || v.warn
	}
	if !warned {
		t.Fatal("never warned")
	}
}

func TestRecvWatchDisk(t *testing.T) {
	var w recvWatch
	if w.diskSample(1 << 30) {
		t.Fatal("1 GB free is not full")
	}
	if !w.diskSample(10 << 20) {
		t.Fatal("10 MB free not reported")
	}
	if w.diskSample(5 << 20) {
		t.Fatal("reported twice")
	}
	// Streaming episodes do not re-arm it; only space coming back does.
	w.reset()
	if w.diskSample(100 << 20) {
		t.Fatal("reported again below the re-arm level")
	}
	w.diskSample(300 << 20)
	if !w.diskSample(1 << 20) {
		t.Fatal("not reported after space came back and went again")
	}
}

func TestNoteErrDiskFullIsPlainAndSaidOnce(t *testing.T) {
	var out bytes.Buffer
	a := &Agent{out: &printer{w: &out}, log: slog.New(slog.DiscardHandler), node: pg.Node{StateDir: "/data/coyote"}}
	for i := range 3 {
		// A new temporary file name every time, as in finding D3.
		a.noteErr(fmt.Errorf("write node info: write /data/coyote/.node.json.tmp-%d: %w", i, syscall.ENOSPC))
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "The disk is full") || !strings.Contains(got, "/data/coyote") || strings.Contains(got, "tmp-") {
		t.Fatalf("output:\n%s", got)
	}
	// Another problem afterwards is still said.
	a.noteErr(errors.New("something else"))
	if !strings.Contains(out.String(), "Problem (will retry): something else") {
		t.Fatalf("output:\n%s", out.String())
	}
}
