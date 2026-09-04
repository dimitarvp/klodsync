package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// manifestPerl is the hub-side emitter, byte-identical to claude_sync's, so the
// hub needs nothing but stock perl. TSV out:
//
//	F <tab> projdir <tab> relpath <tab> size <tab> ts
//	D <tab> satroot <tab> sid
const manifestPerl = `use strict; use warnings; use File::Find;
my $root = $ENV{MROOT}; my $pdir = "$root/projects";
sub iso { my @t = gmtime($_[0]);
  sprintf "%04d-%02d-%02dT%02d:%02d:%02d.000Z",
    $t[5]+1900,$t[4]+1,$t[3],$t[2],$t[1],$t[0]; }
if (-d $pdir) {
  opendir(my $dh, $pdir) or die "opendir $pdir: $!";
  for my $proj (sort readdir $dh) {
    next if $proj =~ /^\./;
    my $p = "$pdir/$proj";
    next unless -d $p; next if -l $p;
    find({ no_chdir => 1, wanted => sub {
      my $f = $File::Find::name;
      return unless -f $f; return if -l $f;
      my $rel = substr($f, length($p) + 1);
      return if $rel =~ /(^|\/)\.DS_Store$/;
      return if $rel =~ /(^|\/)[^\/]*\.lock$/;
      my @st = stat($f); my ($size, $mtime) = ($st[7], $st[9]);
      my $ts = "";
      if ($f =~ /\.jsonl$/ && $size > 0) {
        if (open(my $fh, "<", $f)) {
          my $off = $size > 131072 ? $size - 131072 : 0;
          seek($fh, $off, 0); local $/; my $tail = <$fh>; close $fh;
          while (defined($tail) && $tail =~ /"timestamp"\s*:\s*"([^"]+)"/g) { $ts = $1; }
        }
      }
      $ts = iso($mtime) if $ts eq "";
      print "F\t$proj\t$rel\t$size\t$ts\n";
    }}, $p);
  }
  closedir $dh;
}
for my $satroot (qw(file-history tasks session-env)) {
  my $s = "$root/$satroot"; next unless -d $s;
  opendir(my $sh, $s) or next;
  for my $sid (sort readdir $sh) {
    next if $sid =~ /^\./; next unless -d "$s/$sid";
    print "D\t$satroot\t$sid\n";
  }
  closedir $sh;
}
`

type fileEnt struct {
	Proj string // real dir name on that side
	Rel  string
	Size int64
	TS   string
}

type manifest struct {
	Files []fileEnt
	Sids  map[string]map[string]bool // satroot -> sid -> true
	Lines int                        // total emitted lines, for the scan report
}

var tsRe = regexp.MustCompile(`"timestamp"\s*:\s*"([^"]+)"`)

// lastTS reads the final 128KB and returns the last embedded timestamp — the
// whole-window sweep matters: ~4% of real transcripts end on a non-timestamp
// record (bridge-session, snapshot) with a timestamp earlier in the window.
func lastTS(path string, size int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	const window = 131072
	off := int64(0)
	if size > window {
		off = size - window
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	ms := tsRe.FindAllSubmatch(buf, -1)
	if len(ms) == 0 {
		return ""
	}
	return string(ms[len(ms)-1][1])
}

func isoUTC(t int64) string {
	return timeISO(t)
}

func scanLocal(root string) (*manifest, error) {
	m := &manifest{Sids: map[string]map[string]bool{}}
	pdir := filepath.Join(root, "projects")
	if entries, err := os.ReadDir(pdir); err == nil {
		// os.ReadDir returns entries sorted by name — the order the hub's perl uses too
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			if e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(pdir, name)
			err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // unreadable entries are skipped, like the perl side
				}
				if d.Type()&fs.ModeSymlink != 0 || d.IsDir() {
					return nil
				}
				base := filepath.Base(path)
				if base == ".DS_Store" || strings.HasSuffix(base, ".lock") {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return nil
				}
				rel, _ := filepath.Rel(p, path)
				ts := ""
				if strings.HasSuffix(path, ".jsonl") && info.Size() > 0 {
					ts = lastTS(path, info.Size())
				}
				if ts == "" {
					ts = isoUTC(info.ModTime().Unix())
				}
				m.Files = append(m.Files, fileEnt{Proj: name, Rel: rel, Size: info.Size(), TS: ts})
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	for _, satroot := range []string{"file-history", "tasks", "session-env"} {
		s := filepath.Join(root, satroot)
		entries, err := os.ReadDir(s)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") || !e.IsDir() {
				continue
			}
			sids := m.Sids[satroot]
			if sids == nil {
				sids = map[string]bool{}
				m.Sids[satroot] = sids
			}
			sids[e.Name()] = true
		}
	}
	m.Lines = len(m.Files)
	for _, sids := range m.Sids {
		m.Lines += len(sids)
	}
	return m, nil
}

func parseManifest(text string) *manifest {
	m := &manifest{Sids: map[string]map[string]bool{}}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		switch {
		case parts[0] == "F" && len(parts) == 5:
			var size int64
			size, _ = strconv.ParseInt(parts[3], 10, 64) // unparsable size counts as 0
			m.Files = append(m.Files, fileEnt{Proj: parts[1], Rel: parts[2], Size: size, TS: parts[4]})
			m.Lines++
		case parts[0] == "D" && len(parts) == 3:
			sids := m.Sids[parts[1]]
			if sids == nil {
				sids = map[string]bool{}
				m.Sids[parts[1]] = sids
			}
			sids[parts[2]] = true
			m.Lines++
		}
	}
	return m
}

// ── plan ─────────────────────────────────────────────────────────

type action int

const (
	actSame action = iota
	actPush
	actPushNew
	actPull
	actPullNew
	actDelHub
	actDelLocal
)

// decide is the per-file three-way decision. base ts "" means not in base.
// The returned entry is the side whose timestamp the plan records as the
// agreed state (nil for deletions). Both sides nil is the caller's "gone
// everywhere" case: no action, no entry.
func decide(l, h *fileEnt, baseTS string) (action, *fileEnt) {
	switch {
	case l != nil && h != nil:
		switch {
		case l.TS > h.TS:
			return actPush, l
		case h.TS > l.TS:
			return actPull, h
		default:
			return actSame, l
		}
	case l != nil:
		if baseTS != "" && l.TS <= baseTS {
			return actDelLocal, nil // hub deleted it
		}
		return actPushNew, l
	case h != nil:
		if baseTS != "" && h.TS <= baseTS {
			return actDelHub, nil // local deleted it
		}
		return actPullNew, h
	default:
		return actSame, nil
	}
}

type relTS struct{ Rel, TS string }

type projPlan struct {
	Canon, LDir, HDir string
	LMax, HMax        string
	Push, Pull        []string
	DelHub, DelLocal  []string
	Final             []relTS
	Verdict           string
}

type plan struct {
	Projects []projPlan
	LSids    map[string]map[string]bool
	HSids    map[string]map[string]bool
	LTotal   int // local project-file count, for the mass-delete guard
	HTotal   int
}

func canonOf(name, lpre, hpre string) string {
	if strings.HasPrefix(name, lpre) {
		return strings.TrimPrefix(name, lpre)
	}
	if strings.HasPrefix(name, hpre) {
		return strings.TrimPrefix(name, hpre)
	}
	return name
}

func buildPlan(local, hub *manifest, base map[string]map[string]string, lpre, hpre string) *plan {
	type side struct {
		dir   string
		files map[string]*fileEnt
	}
	group := func(m *manifest) map[string]*side {
		out := map[string]*side{}
		for i := range m.Files {
			f := &m.Files[i]
			c := canonOf(f.Proj, lpre, hpre)
			s := out[c]
			if s == nil {
				s = &side{dir: f.Proj, files: map[string]*fileEnt{}}
				out[c] = s
			}
			s.files[f.Rel] = f
		}
		return out
	}
	L := group(local)
	H := group(hub)

	canonSet := map[string]bool{}
	for c := range L {
		canonSet[c] = true
	}
	for c := range H {
		canonSet[c] = true
	}
	for c := range base {
		canonSet[c] = true
	}
	canons := make([]string, 0, len(canonSet))
	for c := range canonSet {
		canons = append(canons, c)
	}
	sort.Strings(canons)

	p := &plan{LSids: local.Sids, HSids: hub.Sids, LTotal: len(local.Files), HTotal: len(hub.Files)}
	for _, c := range canons {
		ls, hs := L[c], H[c]
		if ls == nil && hs == nil {
			continue // base-only leftovers: nothing exists anywhere any more
		}
		pp := projPlan{Canon: c, LMax: "-", HMax: "-"}
		if ls != nil {
			pp.LDir = ls.dir
		}
		if hs != nil {
			pp.HDir = hs.dir
		}
		rels := map[string]bool{}
		if ls != nil {
			for r := range ls.files {
				rels[r] = true
			}
		}
		if hs != nil {
			for r := range hs.files {
				rels[r] = true
			}
		}
		for r := range base[c] {
			rels[r] = true
		}
		sortedRels := make([]string, 0, len(rels))
		for r := range rels {
			sortedRels = append(sortedRels, r)
		}
		sort.Strings(sortedRels)

		kinds := map[action]bool{}
		for _, r := range sortedRels {
			var l, h *fileEnt
			if ls != nil {
				l = ls.files[r]
			}
			if hs != nil {
				h = hs.files[r]
			}
			if l == nil && h == nil {
				continue // gone both sides
			}
			bts := ""
			if bm := base[c]; bm != nil {
				bts = bm[r]
			}
			act, win := decide(l, h, bts)
			kinds[act] = true
			switch act {
			case actPush, actPushNew:
				pp.Push = append(pp.Push, r)
			case actPull, actPullNew:
				pp.Pull = append(pp.Pull, r)
			case actDelHub:
				pp.DelHub = append(pp.DelHub, r)
			case actDelLocal:
				pp.DelLocal = append(pp.DelLocal, r)
			}
			if win != nil { // the surviving side's timestamp becomes the agreed state
				pp.Final = append(pp.Final, relTS{r, win.TS})
			}
			if l != nil {
				pp.LMax = maxStr(pp.LMax, l.TS)
			}
			if h != nil {
				pp.HMax = maxStr(pp.HMax, h.TS)
			}
		}
		pp.Verdict = verdict(&pp, kinds)
		p.Projects = append(p.Projects, pp)
	}
	return p
}

func maxStr(a, b string) string {
	if a == "-" || b > a {
		return b
	}
	return a
}

func only(kinds map[action]bool, allowed ...action) bool {
	ok := map[action]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range kinds {
		if !ok[k] {
			return false
		}
	}
	return true
}

func verdict(pp *projPlan, kinds map[action]bool) string {
	switch {
	case only(kinds, actSame):
		return "clean"
	case pp.LDir == "" && only(kinds, actSame, actDelHub):
		return "deleted-here"
	case pp.HDir == "" && only(kinds, actSame, actDelLocal):
		return "deleted-on-hub"
	case pp.LDir == "":
		return "new-pull"
	case pp.HDir == "":
		return "new-push"
	case only(kinds, actSame, actPush, actPushNew, actDelHub):
		return "push"
	case only(kinds, actSame, actPull, actPullNew, actDelLocal):
		return "pull"
	default:
		return "union"
	}
}

// ── base state ───────────────────────────────────────────────────

func readBase(path string) (map[string]map[string]string, bool) {
	base := map[string]map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return base, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 3 {
			continue
		}
		proj := base[parts[0]]
		if proj == nil {
			proj = map[string]string{}
			base[parts[0]] = proj
		}
		proj[parts[1]] = parts[2]
	}
	return base, true
}

func writeBase(path string, p *plan) error {
	var b strings.Builder
	for i := range p.Projects {
		pp := &p.Projects[i]
		for _, ft := range pp.Final {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", pp.Canon, ft.Rel, ft.TS)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
