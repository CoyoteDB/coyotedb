package migrate

import (
	"errors"
	"strings"
	"testing"
)

func testRecord() checkRecord {
	return checkRecord{
		Tables: []checkedTable{
			{Table: "public.orders", How: "every row", Rows: 200, Sum: "123456789"},
			{Table: "public.big", How: "first and last 5000 rows", Rows: 9000000, Sum: "42"},
			{Table: "public.nokey", How: "row count", Rows: 7},
		},
		Rows:      9000207,
		Fixed:     1,
		Sequences: []seqValue{{Name: "public.orders_id_seq", Last: 200, Called: true}},
	}
}

func TestCheckRecordRoundTrip(t *testing.T) {
	rec := testRecord()
	rec.Tables = append(rec.Tables, checkedTable{Table: `odd."na{me}'`, How: "every row", Rows: 1, Sum: "1"})
	note, err := rec.comment()
	if err != nil {
		t.Fatal(err)
	}
	if !isCutoverNote(note) || strings.HasPrefix(note, liveComment) {
		t.Fatalf("the record must be told apart from a live copy's comment: %q", note)
	}
	got, ok := parseCheckRecord(note)
	if !ok {
		t.Fatalf("record not read back from %q", note)
	}
	if len(got.Tables) != len(rec.Tables) || got.Tables[3] != rec.Tables[3] || got.Rows != rec.Rows || got.Fixed != 1 || got.Sequences[0] != rec.Sequences[0] {
		t.Fatalf("round trip changed the record: %+v", got)
	}
}

func TestParseCheckRecordRefusesWhatItCannotTrust(t *testing.T) {
	good, _ := testRecord().comment()
	for name, note := range map[string]string{
		"empty":            "",
		"one-time copy":    "Unfinished copy made by coyote migrate. Not in use; safe to drop.",
		"live copy":        liveComment + " from db.example.com:5432/shop, started 2026-11-22 10:00 UTC. Not in use until the cutover.",
		"cut short":        good[:len(good)-5],
		"no record":        cutoverComment + ". The copy was checked.",
		"other version":    strings.Replace(good, `"v":1`, `"v":2`, 1),
		"no tables":        cutoverComment + ` {"v":1,"tables":[],"rows":0}`,
		"moved in":         movedComment + "2026-11-22.",
		"not at the start": "x" + good,
	} {
		if _, ok := parseCheckRecord(note); ok {
			t.Errorf("%s: a record was read from %q", name, note)
		}
	}
}

func TestDecideLive(t *testing.T) {
	marker, _ := testRecord().comment()
	live := liveComment + " from h:5432/shop, started 2026-11-22 10:00 UTC. Not in use until the cutover."
	oneTime := "Unfinished copy made by coyote migrate. Not in use; safe to drop."
	moved := movedComment + "2026-11-22."
	for _, c := range []struct {
		name string
		o    liveObs
		want liveStep
	}{
		{"nothing at all", liveObs{}, stepNoLive},
		{"an app database never moved into", liveObs{target: true}, stepNoLive},
		{"live copy running", liveObs{staging: true, stagingNote: live, sub: true, subSlot: true, target: true}, stepFull},
		{"live copy stopped by an earlier run (disabled)", liveObs{staging: true, stagingNote: live, sub: true, subSlot: true}, stepFull},
		{"killed after the check was recorded", liveObs{staging: true, stagingNote: marker, sub: true, subSlot: true, target: true}, stepResume},
		{"killed while removing the subscription", liveObs{staging: true, stagingNote: marker, sub: true, subSlot: false, target: true}, stepResume},
		{"killed after the subscription was removed", liveObs{staging: true, stagingNote: marker, target: true}, stepResume},
		{"killed between the two renames", liveObs{staging: true, stagingNote: marker, replaced: true}, stepResume},
		{"killed after the copy took the name", liveObs{target: true, targetNote: marker, replaced: true}, stepDone},
		{"killed before the old contents were dropped", liveObs{target: true, targetNote: moved, replaced: true}, stepDone},
		{"switch finished", liveObs{target: true, targetNote: moved}, stepDone},
		{"unfinished one-time copy", liveObs{staging: true, stagingNote: oneTime, target: true}, stepNotLive},
		{"cancel stopped after the subscription went", liveObs{staging: true, stagingNote: live, target: true}, stepNotLive},
		{"cancel stopped with the subscription detached", liveObs{staging: true, stagingNote: live, sub: true, target: true}, stepDetached},
		{"damaged record, subscription still there", liveObs{staging: true, stagingNote: marker[:len(marker)-3], sub: true, subSlot: true}, stepFull},
		{"damaged record, no subscription", liveObs{staging: true, stagingNote: marker[:len(marker)-3]}, stepNotLive},
	} {
		if got := decideLive(c.o); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestChangedSince(t *testing.T) {
	rec := testRecord()
	tables := map[string]table{
		"public.orders": {Schema: "public", Name: "orders", Size: 1 << 40}, // grown, still checked every row
		"public.big":    {Schema: "public", Name: "big", PK: []string{"id"}, PKPlain: true},
		"public.nokey":  {Schema: "public", Name: "nokey"},
	}
	now := map[string]tableStats{
		"public.orders": {Rows: 200, Sum: "123456789"},
		"public.big":    {Rows: 9000000, Sum: "42"},
		"public.nokey":  {Rows: 7},
	}
	var modes []string
	stats := func(t table) (tableStats, error) {
		modes = append(modes, t.label()+"="+checkMode(t))
		return now[t.label()], nil
	}
	seqs := []seqValue{{Name: "public.orders_id_seq", Last: 200, Called: true}}

	diff, err := changedSince(&rec, tables, stats, seqs)
	if err != nil || len(diff) != 0 {
		t.Fatalf("unchanged copy: %v %v", diff, err)
	}
	want := []string{"public.orders=every row", "public.big=first and last 5000 rows", "public.nokey=row count"}
	if strings.Join(modes, ",") != strings.Join(want, ",") {
		t.Fatalf("not checked the way it was checked then: %v", modes)
	}

	now["public.orders"] = tableStats{Rows: 201, Sum: "1"}
	now["public.nokey"] = tableStats{Rows: 7, Sum: "x"}
	delete(tables, "public.big")
	seqs = []seqValue{{Name: "public.orders_id_seq", Last: 201, Called: true}, {Name: "public.new_seq", Last: 1}}
	diff, err = changedSince(&rec, tables, stats, seqs)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(diff, "\n")
	for _, w := range []string{
		"public.orders: 200 rows when checked, 201 now",
		"public.big: no longer in the copy",
		"public.nokey: same number of rows (7) but different contents",
		"sequence public.orders_id_seq: at 200 when checked, 201 now",
		"sequence public.new_seq: new since the check",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}

	// A primary key that no longer sorts the same everywhere cannot be
	// checked the same way: that is a change too.
	tables["public.big"] = table{Schema: "public", Name: "big", PK: []string{"code"}, PKPlain: false}
	diff, _ = changedSince(&rec, tables, stats, nil)
	if !strings.Contains(strings.Join(diff, "\n"), "public.big: its primary key changed") {
		t.Errorf("a changed key was not reported: %v", diff)
	}
	if !strings.Contains(strings.Join(diff, "\n"), "sequence public.orders_id_seq: no longer in the copy") {
		t.Errorf("a removed sequence was not reported: %v", diff)
	}

	boom := errors.New("boom")
	if _, err := changedSince(&rec, tables, func(table) (tableStats, error) { return tableStats{}, boom }, nil); !errors.Is(err, boom) {
		t.Errorf("a failed read must stop the check, got %v", err)
	}
}
