package main

import (
	"path/filepath"
	"testing"
)

// flock is per open file description, so a second OpenFile in the same
// process is a distinct locker: the test needs no child processes.
func TestAcquireLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "klodsync.lock")

	ex, err := acquireLock(path, false)
	if err != nil {
		t.Fatalf("first exclusive lock: %v", err)
	}
	if f, err := acquireLock(path, false); err == nil {
		_ = f.Close()
		t.Fatal("second exclusive lock succeeded while the first is held")
	}
	if f, err := acquireLock(path, true); err == nil {
		_ = f.Close()
		t.Fatal("shared lock succeeded while an exclusive one is held")
	}
	_ = ex.Close() // release = close, exactly what process exit does

	sh1, err := acquireLock(path, true)
	if err != nil {
		t.Fatalf("shared lock after release: %v", err)
	}
	sh2, err := acquireLock(path, true)
	if err != nil {
		t.Fatalf("second shared lock alongside the first: %v", err)
	}
	if f, err := acquireLock(path, false); err == nil {
		_ = f.Close()
		t.Fatal("exclusive lock succeeded while shared locks are held")
	}
	_ = sh1.Close()
	_ = sh2.Close()

	ex2, err := acquireLock(path, false)
	if err != nil {
		t.Fatalf("exclusive lock after the shared ones closed: %v", err)
	}
	_ = ex2.Close()
}
