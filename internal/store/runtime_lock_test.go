package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qqcodex/internal/model"
)

func TestAcquireRuntimeLockRejectsUnsafeLockFile(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{"symlink", func(t *testing.T, path string) {
			target := path + ".target"
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra hard link", func(t *testing.T, path string) {
			target := path + ".target"
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"group readable", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"owner executable", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := openTestStore(t)
			test.prepare(t, db.path+runtimeLockSuffix)
			if lock, err := db.AcquireRuntimeLock(); err == nil {
				_ = lock.Close()
				t.Fatal("AcquireRuntimeLock() accepted unsafe lock file")
			}
		})
	}
}

func TestRuntimeLockGuardsInterruptedRecoveryAcrossStores(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tasks.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	now := time.Now().UTC()
	task := testTask("runtime-lock-deploy", model.StatusDeploying, now)
	task.RCCommit = strings.Repeat("c", 40)
	task.DeployKey = "persisted-key"
	if err := first.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := first.AddApproval(ctx, model.Approval{
		TaskID: task.ID, Kind: "deploy", UserID: "admin", GroupID: "group",
		MessageID: "runtime-lock", BoundCommit: task.RCCommit, Result: "approved", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ClaimApproval(ctx, task.ID, "deploy", model.StatusDeploying, task.RCCommit, now); err != nil {
		t.Fatal(err)
	}

	firstLock, err := first.AcquireRuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstLock.Close() })
	if _, err := second.AcquireRuntimeLock(); !errors.Is(err, ErrRuntimeLocked) {
		t.Fatalf("second AcquireRuntimeLock() error = %v, want ErrRuntimeLocked", err)
	}
	if err := second.RecoverInterrupted(ctx, firstLock); !errors.Is(err, ErrRuntimeLockRequired) {
		t.Fatalf("second RecoverInterrupted() error = %v, want ErrRuntimeLockRequired", err)
	}
	assertTaskAndApprovalLock(t, second, task.ID, model.StatusDeploying, 1)

	if err := firstLock.Close(); err != nil {
		t.Fatal(err)
	}
	secondLock, err := second.AcquireRuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondLock.Close() })
	if err := second.RecoverInterrupted(ctx, secondLock); err != nil {
		t.Fatal(err)
	}
	assertTaskAndApprovalLock(t, second, task.ID, model.StatusDeployFailed, 0)
}

func TestRecoverInterruptedRequiresHeldRuntimeLock(t *testing.T) {
	db := openTestStore(t)
	if err := db.RecoverInterrupted(context.Background(), nil); !errors.Is(err, ErrRuntimeLockRequired) {
		t.Fatalf("RecoverInterrupted(nil) error = %v, want ErrRuntimeLockRequired", err)
	}
	lock, err := db.AcquireRuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverInterrupted(context.Background(), lock); !errors.Is(err, ErrRuntimeLockRequired) {
		t.Fatalf("RecoverInterrupted(closed) error = %v, want ErrRuntimeLockRequired", err)
	}
}

func TestStoreCloseDoesNotCloseRuntimeLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := first.AcquireRuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := second.AcquireRuntimeLock(); !errors.Is(err, ErrRuntimeLocked) {
		t.Fatalf("AcquireRuntimeLock() after Store.Close error = %v, want ErrRuntimeLocked", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	secondLock, err := second.AcquireRuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondLock.Close() })
}

func assertTaskAndApprovalLock(t *testing.T, db *Store, taskID string, status model.Status, lockCount int) {
	t.Helper()
	task, err := db.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != status {
		t.Fatalf("task status = %s, want %s", task.Status, status)
	}
	var got int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM approval_execution_locks").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != lockCount {
		t.Fatalf("approval lock count = %d, want %d", got, lockCount)
	}
}

func acquireTestRuntimeLock(t *testing.T, db *Store) *RuntimeLock {
	t.Helper()
	lock, err := db.AcquireRuntimeLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}
