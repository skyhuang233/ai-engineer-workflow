package startup

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireLockRejectsConcurrentControlPlaneAndReleasesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	first, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreBarrierDrainsDatabaseAccessAndBlocksNewAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	first, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireRestoreBarrier(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restore barrier while database is active = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	restore, err := AcquireRestoreBarrier(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireDatabaseAccess(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("database access during restore = %v", err)
	}
	if err := restore.Close(); err != nil {
		t.Fatal(err)
	}
	access, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreBarrierWriterIntentBlocksLaterReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	reader, err := AcquireDatabaseAccess(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	writerAcquired := make(chan *Lock, 1)
	writerErrors := make(chan error, 1)
	go func() {
		writer, err := AcquireRestoreBarrier(context.Background(), path)
		if err != nil {
			writerErrors <- err
			return
		}
		writerAcquired <- writer
	}()
	identity, err := DatabaseIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := openLockFile(identity + ".restore.intent.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	deadline := time.Now().Add(time.Second)
	for {
		err := tryLockFile(probe, false)
		if isLockConflict(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := unlockFile(probe); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("restore writer did not publish intent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireDatabaseAccess(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("later reader bypassed restore intent: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writerErrors:
		t.Fatal(err)
	case writer := <-writerAcquired:
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("restore writer did not acquire drained barrier")
	}
}

func TestDatabaseBarrierCanonicalizesFileURIAndSymlinkAliases(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "workflow.db")
	fileURI := (&url.URL{Scheme: "file", Path: "/" + strings.TrimLeft(filepath.ToSlash(path), "/")}).String()
	access, err := AcquireDatabaseAccess(context.Background(), fileURI)
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if _, err := AcquireRestoreBarrier(blocked, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("plain path bypassed file URI barrier: %v", err)
	}
	cancel()
	if err := access.Close(); err != nil {
		t.Fatal(err)
	}
	realDirectory := filepath.Join(directory, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDirectory := filepath.Join(directory, "alias")
	if err := os.Symlink(realDirectory, aliasDirectory); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	access, err = AcquireDatabaseAccess(context.Background(), filepath.Join(realDirectory, "aliased.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	blocked, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireRestoreBarrier(blocked, filepath.Join(aliasDirectory, "aliased.db")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("symlink alias bypassed database barrier: %v", err)
	}
}
