package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fe(ts string) *fileEnt { return &fileEnt{TS: ts} }

// The per-file three-way decision — the part of the program where a wrong
// branch loses data.
func TestDecide(t *testing.T) {
	const older, newer, base = "2026-01-01T00:00:00.000Z", "2026-06-01T00:00:00.000Z", "2026-03-01T00:00:00.000Z"
	cases := []struct {
		name   string
		l, h   *fileEnt
		baseTS string
		want   action
	}{
		{"both equal ts -> same (even with different bytes)", fe(older), fe(older), "", actSame},
		{"local newer -> push", fe(newer), fe(older), "", actPush},
		{"hub newer -> pull", fe(older), fe(newer), "", actPull},
		{"local only, no base -> new push", fe(older), nil, "", actPushNew},
		{"hub only, no base -> new pull", nil, fe(older), "", actPullNew},
		{"local only, unchanged since base -> hub deleted it", fe(base), nil, base, actDelLocal},
		{"local only, advanced past base -> modify beats delete", fe(newer), nil, base, actPushNew},
		{"hub only, unchanged since base -> local deleted it", nil, fe(base), base, actDelHub},
		{"hub only, advanced past base -> resurrect", nil, fe(newer), base, actPullNew},
		{"local only, OLDER than base -> still deletion", fe(older), nil, base, actDelLocal},
	}
	for _, c := range cases {
		if got := decide(c.l, c.h, c.baseTS); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestVerdicts(t *testing.T) {
	mk := func(ldir, hdir string, acts ...action) string {
		kinds := map[action]bool{}
		for _, a := range acts {
			kinds[a] = true
		}
		return verdict(&projPlan{LDir: ldir, HDir: hdir}, kinds)
	}
	cases := []struct{ got, want string }{
		{mk("d", "d", actSame), "clean"},
		{mk("d", "d"), "clean"}, // empty kinds
		{mk("", "d", actDelHub), "deleted-here"},
		{mk("d", "", actDelLocal), "deleted-on-hub"},
		{mk("", "d", actPullNew), "new-pull"},
		{mk("d", "", actPushNew), "new-push"},
		{mk("d", "d", actSame, actPush, actDelHub), "push"},
		{mk("d", "d", actSame, actPull, actDelLocal), "pull"},
		{mk("d", "d", actPush, actPull), "union"},
		// partial delete + advanced file with no local dir: bash's verdict order
		// labels this new-pull (actions still delete AND pull) — parity over taste
		{mk("", "d", actDelHub, actPullNew), "new-pull"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %q want %q", i, c.got, c.want)
		}
	}
}

// The measured porting hazards: last line without a timestamp (timestamp
// earlier in the window), no timestamp anywhere (mtime fallback), >64KB lines.
func TestLastTS(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	p := write("normal.jsonl", `{"a":1,"timestamp":"2026-01-01T00:00:00.000Z"}`+"\n"+
		`{"a":2,"timestamp":"2026-02-02T00:00:00.000Z"}`+"\n")
	if got := lastTS(p, fsize(t, p)); got != "2026-02-02T00:00:00.000Z" {
		t.Errorf("normal: %q", got)
	}

	// last line has NO timestamp — must still find the one before it
	p = write("tailless.jsonl", `{"timestamp":"2026-03-03T00:00:00.000Z"}`+"\n"+
		`{"agentId":"x","key":"y","result":"z","type":"w"}`+"\n")
	if got := lastTS(p, fsize(t, p)); got != "2026-03-03T00:00:00.000Z" {
		t.Errorf("tailless: %q", got)
	}

	// no timestamp anywhere -> "" (caller falls back to mtime)
	p = write("none.jsonl", `{"a":1}`+"\n{}"+"\n")
	if got := lastTS(p, fsize(t, p)); got != "" {
		t.Errorf("none: %q", got)
	}

	// a single line far larger than 64KB (bufio.Scanner would choke)
	big := `{"pad":"` + strings.Repeat("x", 200_000) + `","timestamp":"2026-04-04T00:00:00.000Z"}` + "\n"
	p = write("big.jsonl", big)
	if got := lastTS(p, fsize(t, p)); got != "2026-04-04T00:00:00.000Z" {
		t.Errorf("big line: %q", got)
	}

	// timestamp OUTSIDE the 128KB window, none inside -> "" (window is the contract)
	outside := `{"timestamp":"2026-05-05T00:00:00.000Z"}` + "\n" +
		`{"pad":"` + strings.Repeat("y", 200_000) + `"}` + "\n"
	p = write("outside.jsonl", outside)
	if got := lastTS(p, fsize(t, p)); got != "" {
		t.Errorf("outside window: %q (want empty -> mtime fallback)", got)
	}
}

func fsize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestMungeCanon(t *testing.T) {
	if got := munge("/home/dimi/.local/share/chezmoi"); got != "-home-dimi--local-share-chezmoi" {
		t.Errorf("munge: %q", got)
	}
	lpre, hpre := munge("/home/dimi"), munge("/Users/dimi")
	if got := canonOf("-home-dimi-kod-x", lpre, hpre); got != "-kod-x" {
		t.Errorf("canon local: %q", got)
	}
	if got := canonOf("-Users-dimi-kod-x", lpre, hpre); got != "-kod-x" {
		t.Errorf("canon hub: %q", got)
	}
	if got := canonOf("-var-home-user-x", lpre, hpre); got != "-var-home-user-x" {
		t.Errorf("canon foreign: %q", got)
	}
}

func TestOrderedProjects(t *testing.T) {
	raw := []byte(`{"other":1,"projects":{"/b/two":{"lastSessionId":"2"},"/a/one":{"lastSessionId":"1"}},"tail":true}`)
	got := orderedProjects(raw)
	if len(got) != 2 || got[0].Key != "/b/two" || got[1].Key != "/a/one" {
		t.Fatalf("order not preserved: %+v", got)
	}
}

func TestValidateDeletes(t *testing.T) {
	good := []string{
		"projects/-home-dimi-x/abc.jsonl",
		"projects/-home-dimi-x/memory/f.md",
		"file-history/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa/",
		"tasks/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa/",
	}
	if err := validateDeletes(good); err != nil {
		t.Errorf("good paths refused: %v", err)
	}
	for _, bad := range []string{
		"/etc/passwd",
		"projects/../../etc",
		"file-history/short/",
		"history.jsonl",
		"projects/onlydir",
	} {
		if err := validateDeletes([]string{bad}); err == nil {
			t.Errorf("bad path accepted: %s", bad)
		}
	}
}
