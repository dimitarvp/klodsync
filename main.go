// klodsync — per-project bidirectional Claude Code data sync (Go port of claude_sync).
//
//	klodsync status              show per-project verdicts, change nothing
//	klodsync run [--dry-run]     sync both directions, per project
//	klodsync adopt <project>     make a foreign-machine project resumable here
//
// Direction is decided per project, per FILE, by newest activity (timestamps
// read from inside session files, not mtimes). Newer history is never
// overwritten by older history, in either direction.
//
// Divergence (unique sessions on both sides) merges by union: fill what's
// missing, take the newer copy of genuinely-changed shared files, delete
// nothing, report loudly.
//
// Deletions propagate via a per-hub state file recording the agreed file list
// after the last successful run (~/.claude/.claude-sync/). No state => no
// deletions (pure merge). State is written only after a fully successful run.
//
// Hub: --hub HOST:DIR (DIR relative to the remote $HOME) or a local DIR.
// Default robeast:.claude, override with $CLAUDE_SYNC_HUB.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	Hub             string
	LRoot           string // local ~/.claude
	LJSON           string // local ~/.claude.json
	LHome           string
	HHost           string // empty in local-dir mode
	HRoot           string
	HHome           string
	HJSON           string
	LPrefix         string
	HPrefix         string
	Remote          bool
	Dry             bool
	AllowMassDelete bool

	Work     string
	StateDir string
	BaseFile string
	LockDir  string
	SSHOpts  []string
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func munge(p string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' {
			return '-'
		}
		return r
	}, p)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: klodsync status|run [--dry-run] [--hub HOST:DIR|DIR] [--allow-mass-delete]")
	fmt.Fprintln(os.Stderr, "       klodsync adopt <project-dir-name>")
	os.Exit(2)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
	}
	verb := args[0]
	args = args[1:]
	adoptTarget := ""

	switch verb {
	case "status", "run", "adopt":
	default:
		usage()
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fatal("cannot determine home: %v", err)
	}
	cfg := &config{
		Hub:   envOr("CLAUDE_SYNC_HUB", "robeast:.claude"),
		LRoot: envOr("CLAUDE_SYNC_LOCAL_CLAUDE", filepath.Join(home, ".claude")),
		LJSON: envOr("CLAUDE_SYNC_LOCAL_JSON", filepath.Join(home, ".claude.json")),
		LHome: envOr("CLAUDE_SYNC_LOCAL_HOME", home),
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run", "-n":
			cfg.Dry = true
		case "--allow-mass-delete":
			cfg.AllowMassDelete = true
		case "--hub":
			i++
			if i >= len(args) {
				fatal("--hub needs a value")
			}
			cfg.Hub = args[i]
		default:
			// munged project names start with "-"; in adopt mode a non-flag arg is the target
			if verb == "adopt" && adoptTarget == "" {
				adoptTarget = args[i]
			} else {
				fatal("unknown flag: %s", args[i])
			}
		}
	}
	if verb == "status" {
		cfg.Dry = true
	}
	if verb == "adopt" && adoptTarget == "" {
		usage()
	}

	cfg.SSHOpts = []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + home + "/.ssh/cs-%r@%h",
		"-o", "ControlPersist=120",
		"-o", "ConnectTimeout=10",
	}

	// hub plumbing
	if i := strings.Index(cfg.Hub, ":"); i >= 0 {
		cfg.Remote = true
		cfg.HHost = cfg.Hub[:i]
		cfg.HRoot = cfg.Hub[i+1:]
		hh := os.Getenv("CLAUDE_SYNC_HUB_HOME")
		if hh == "" {
			out, err := exec.Command("ssh", append(cfg.SSHOpts, cfg.HHost, `echo "$HOME"`)...).Output()
			if err != nil {
				fatal("cannot reach hub %s: %v", cfg.HHost, err)
			}
			hh = strings.TrimSpace(string(out))
		}
		cfg.HHome = hh
		switch {
		case strings.HasPrefix(cfg.HRoot, "/"):
		case strings.HasPrefix(cfg.HRoot, "~/"):
			cfg.HRoot = cfg.HHome + "/" + strings.TrimPrefix(cfg.HRoot, "~/")
		default:
			cfg.HRoot = cfg.HHome + "/" + cfg.HRoot
		}
	} else {
		cfg.HHome = envOr("CLAUDE_SYNC_HUB_HOME", cfg.LHome)
		abs, err := filepath.Abs(cfg.Hub)
		if err != nil {
			fatal("bad hub dir: %v", err)
		}
		cfg.HRoot = abs
	}
	cfg.HJSON = cfg.HRoot + ".json"
	cfg.LPrefix = munge(cfg.LHome)
	cfg.HPrefix = munge(cfg.HHome)

	cfg.StateDir = filepath.Join(cfg.LRoot, ".claude-sync")
	cfg.BaseFile = filepath.Join(cfg.StateDir, "base."+hubKey(cfg)+".tsv")
	cfg.LockDir = filepath.Join(cfg.StateDir, "lock")

	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		fatal("state dir: %v", err)
	}

	// the work dir must share a filesystem with ~/.claude and ~/.claude.json:
	// staged files are moved into place with rename(2), which cannot cross
	// devices (/tmp is tmpfs on some machines). Crashed runs can leave work.*
	// behind, so old ones are swept like stale locks.
	if old, globErr := filepath.Glob(filepath.Join(cfg.StateDir, "work.*")); globErr == nil {
		for _, d := range old {
			if st, statErr := os.Stat(d); statErr == nil && time.Since(st.ModTime()) > 2*time.Hour {
				_ = os.RemoveAll(d)
			}
		}
	}
	work, err := os.MkdirTemp(cfg.StateDir, "work.")
	if err != nil {
		fatal("mktemp: %v", err)
	}
	cfg.Work = work

	// one run at a time per machine (stale lock from a crash is removed by age)
	if err := os.Mkdir(cfg.LockDir, 0o755); err != nil {
		st, serr := os.Stat(cfg.LockDir)
		if serr == nil && time.Since(st.ModTime()) > 2*time.Hour {
			fmt.Println("removing stale lock (>2h old)")
			_ = os.Remove(cfg.LockDir)
			if err2 := os.Mkdir(cfg.LockDir, 0o755); err2 != nil {
				fatal("another klodsync/claude_sync is running (%s); aborting", cfg.LockDir)
			}
		} else {
			fatal("another klodsync/claude_sync is running (%s); aborting", cfg.LockDir)
		}
	}
	defer func() {
		_ = os.RemoveAll(cfg.Work)
		_ = os.Remove(cfg.LockDir)
	}()

	var runErr error
	if verb == "adopt" {
		runErr = adopt(cfg, adoptTarget)
	} else {
		runErr = sync(cfg, verb)
	}
	if runErr != nil {
		// cleanup runs via defer
		fmt.Fprintf(os.Stderr, "klodsync: %v\n", runErr)
		_ = os.RemoveAll(cfg.Work)
		_ = os.Remove(cfg.LockDir)
		os.Exit(1)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "klodsync: "+format+"\n", a...)
	os.Exit(1)
}

// hubKey matches the bash tool exactly: `echo "host:root" | cksum | cut -d' ' -f1`
// so klodsync and claude_sync share state files.
func hubKey(cfg *config) string {
	cmd := exec.Command("cksum")
	cmd.Stdin = strings.NewReader(cfg.HHost + ":" + cfg.HRoot + "\n")
	out, err := cmd.Output()
	if err != nil {
		fatal("cksum: %v", err)
	}
	return strings.Fields(string(out))[0]
}
