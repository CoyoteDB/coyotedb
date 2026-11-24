package migrate

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// tocEntry is one line of `pg_restore -l`:
//
//	224; 1259 16468 TABLE public items postgres
//	3921; 0 0 COMMENT - EXTENSION pg_stat_statements
//
// The description ("TABLE DATA", "FK CONSTRAINT", ...) may contain spaces,
// so only the kinds this package acts on are recognised; every other line
// is kept as it is.
type tocEntry struct {
	Line   string // the whole line, as pg_restore wrote it
	ID     int    // dump id
	Desc   string // recognised kind, or "" for anything else
	Schema string // "-" when the object has none
	Name   string // the object name (for a COMMENT: the rest of the line)
}

// Kinds the copy treats specially.
var knownDescs = []string{
	"TABLE DATA", "SCHEMA", "EXTENSION", "COMMENT", "EVENT TRIGGER",
	"PUBLICATION", "SUBSCRIPTION", "MATERIALIZED VIEW DATA", "POLICY",
}

func parseTOC(r io.Reader) ([]tocEntry, error) {
	var out []tocEntry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		e := tocEntry{Line: line}
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, ";") {
			e = parseTOCLine(line)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func parseTOCLine(line string) tocEntry {
	e := tocEntry{Line: line}
	idStr, rest, ok := strings.Cut(line, ";")
	if !ok {
		return e
	}
	id, err := strconv.Atoi(strings.TrimSpace(idStr))
	if err != nil {
		return e
	}
	e.ID = id
	// "<tableoid> <oid> <desc> <schema> <name> <owner>"
	f := strings.Fields(rest)
	if len(f) < 3 {
		return e
	}
	tail := strings.Join(f[2:], " ")
	for _, d := range knownDescs {
		if tail == d || strings.HasPrefix(tail, d+" ") {
			// Longest match first: "MATERIALIZED VIEW DATA" vs others is
			// unambiguous; "TABLE DATA" must not be read as "TABLE".
			e.Desc = d
			after := strings.Fields(strings.TrimPrefix(tail, d))
			if len(after) > 0 {
				e.Schema = after[0]
			}
			if len(after) > 1 {
				switch {
				case d == "COMMENT":
					e.Name = strings.Join(after[1:], " ")
				case d == "POLICY" && len(after) > 2:
					// "<table> <policy name, may have spaces> <owner>"
					e.Name = strings.Join(after[1:len(after)-1], " ")
				default:
					e.Name = after[1]
				}
			}
			break
		}
	}
	return e
}

// isComment reports whether the line is an item (not a ';' comment line).
func (e tocEntry) isItem() bool { return e.ID != 0 }

// extensionComment: `COMMENT - EXTENSION name`.
func (e tocEntry) extensionComment() bool {
	return e.Desc == "COMMENT" && e.Schema == "-" && strings.HasPrefix(e.Name, "EXTENSION ")
}

// tocPlan splits the archive's items into what is restored and what is
// left out, and the order: schemas first (so extensions created by the
// administrator have somewhere to go), then everything else.
type tocPlan struct {
	schemas  []tocEntry // SCHEMA items
	rest     []tocEntry // everything else that is restored
	leftOut  []string   // plain-words list of what was not restored
	dataByID map[int]tocEntry
}

func planTOC(entries []tocEntry, dropPolicies map[string]bool) tocPlan {
	p := tocPlan{dataByID: map[int]tocEntry{}}
	for _, e := range entries {
		if !e.isItem() {
			continue
		}
		switch {
		case e.Desc == "SCHEMA":
			p.schemas = append(p.schemas, e)
			continue
		case e.extensionComment():
			// The extension's own description. It belongs to whoever
			// created the extension, which may be the administrator.
			continue
		case e.Desc == "EVENT TRIGGER":
			p.leftOut = append(p.leftOut, "event trigger "+e.Name+" (needs the database administrator)")
			continue
		case e.Desc == "PUBLICATION", e.Desc == "SUBSCRIPTION":
			continue // never dumped (--no-publications/--no-subscriptions)
		case e.Desc == "POLICY" && dropPolicies[e.Schema+"\x00"+e.Name]:
			continue // for a role the cluster lacks; listed in the plan
		case e.Desc == "TABLE DATA", e.Desc == "MATERIALIZED VIEW DATA":
			p.dataByID[e.ID] = e
		}
		p.rest = append(p.rest, e)
	}
	return p
}

func listFile(entries []tocEntry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Line)
		b.WriteByte('\n')
	}
	return b.String()
}
