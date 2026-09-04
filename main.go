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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
	LockFile string
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
	cfg.LockFile = filepath.Join(cfg.StateDir, "klodsync.lock")

	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		fatal("state dir: %v", err)
	}

	// A closed pager (klodsync status | head) must not kill a run half-way:
	// with SIGPIPE ignored, writes to the dead pipe fail quietly and the run
	// completes. The lock below is released by the kernel either way.
	signal.Ignore(syscall.SIGPIPE)

	os.Exit(runLocked(cfg, verb, adoptTarget))
}

// runLocked takes the per-machine lock, does the work and returns the exit
// code; every deferred cleanup runs before main exits.
func runLocked(cfg *config, verb, adoptTarget string) int {
	// The bash oracle (claude_sync) locks with a mkdir at StateDir/lock. If
	// that dir exists it is a live oracle run or its leftover: refuse, never
	// guess from its age.
	legacy := filepath.Join(cfg.StateDir, "lock")
	if _, err := os.Stat(legacy); err == nil {
		fatal("legacy lock dir %s exists: a claude_sync (bash) run holds it, or it is a leftover; if `pgrep -x claude_sync` shows nothing, remove the dir and retry", legacy)
	}
	lf, err := acquireLock(cfg.LockFile, verb == "status")
	if err != nil {
		if verb == "status" {
			fatal("a klodsync run is in progress on this machine; retry when it finishes")
		}
		fatal("another klodsync (run or status) is active on this machine; aborting")
	}
	defer func() { _ = lf.Close() }() // releases the lock; the kernel does the same on any exit

	// The work dir must share a filesystem with ~/.claude and ~/.claude.json:
	// staged files are moved into place with rename(2), which cannot cross
	// devices (/tmp is tmpfs on some machines). Under the exclusive lock no
	// other run is alive, so every work.* present is the orphan of a crashed
	// run: sweep them all, no age guessing.
	if verb != "status" {
		if old, globErr := filepath.Glob(filepath.Join(cfg.StateDir, "work.*")); globErr == nil {
			for _, d := range old {
				_ = os.RemoveAll(d)
			}
		}
	}
	work, err := os.MkdirTemp(cfg.StateDir, "work.")
	if err != nil {
		fatal("mktemp: %v", err)
	}
	cfg.Work = work
	defer func() { _ = os.RemoveAll(work) }()

	var runErr error
	if verb == "adopt" {
		runErr = adopt(cfg, adoptTarget)
	} else {
		runErr = sync(cfg, verb)
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "klodsync: %v\n", runErr)
		return 1
	}
	return 0
}

// acquireLock takes an advisory kernel lock (flock) on path, shared for
// readers and exclusive for writers, without waiting. The file is created
// once and never removed: flock binds to the inode, so deleting and
// re-creating it would let two processes hold "the lock" at once. The kernel
// releases the lock when the holder exits, however it exits (SIGPIPE, kill
// -9), so there is nothing stale to detect or reclaim. The returned file must
// stay open for as long as the lock is needed.
func acquireLock(path string, shared bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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
