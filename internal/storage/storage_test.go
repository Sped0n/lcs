package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openStore(t *testing.T, limit int64) (*Store, string, string) {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	root := filepath.Join(base, "cache")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(state, root, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	return store, state, root
}

func publish(t *testing.T, s *Store, key string, size int64) Entry {
	t.Helper()
	r, err := s.Reserve(size)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Abort(); err != nil {
			t.Error(err)
		}
	}()

	if err := os.WriteFile(filepath.Join(r.Path, "body"), []byte(strings.Repeat("x", int(size))), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.Publish(key, "{}"); err != nil {
		t.Fatal(err)
	}

	e, release, ok, err := s.Acquire(key)
	if err != nil || !ok {
		t.Fatalf("acquire %s: %v %v", key, ok, err)
	}

	release()
	return e
}

func TestLRUAndActiveProtection(t *testing.T) {
	s, _, _ := openStore(t, 1000)
	first := publish(t, s, "first", 300)
	publish(t, s, "second", 300)

	_, release, found, err := s.Acquire("first")
	if err != nil || !found {
		t.Fatal(err)
	}

	publish(t, s, "third", 300)
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatal("active entry evicted:", err)
	}

	_, free, found, err := s.Acquire("second")
	if err != nil {
		t.Fatal(err)
	}
	free()
	if found {
		t.Fatal("LRU entry not evicted")
	}

	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatal("clear deleted active entry")
	}

	_, free, found, err = s.Acquire("first")
	if err != nil {
		t.Fatal(err)
	}
	free()
	if found {
		t.Fatal("cleared active entry admitted another reader")
	}

	release()
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatal("pending entry not removed")
	}
}

func TestReservationsAndClearGeneration(t *testing.T) {
	s, _, root := openStore(t, 1000)
	if _, err := s.Reserve(1001); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	first, err := s.Reserve(700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(400); !errors.Is(err, ErrCapacity) {
		t.Fatal("overcommitted reservations:", err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "body"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Temporary != 6 {
		t.Fatal(stats)
	}

	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}

	if err := first.Publish("key", ""); err == nil {
		t.Fatal("published a download from before clear")
	}
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}

	files, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil || len(files) != 0 {
		t.Fatal("staging left behind:", err)
	}
}

func TestOversizePublicationAndReplacement(t *testing.T) {
	s, _, _ := openStore(t, 1000)
	original := publish(t, s, "key", 50)

	_, release, _, err := s.Acquire("key")
	if err != nil {
		t.Fatal(err)
	}

	r, err := s.Reserve(20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Path, "body"), []byte(strings.Repeat("x", 21)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish("key", ""); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Path, "body"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish("key", ""); err == nil {
		t.Fatal("replaced active generation")
	}

	release()
	if err := r.Publish("key", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(original.Path); !os.IsNotExist(err) {
		t.Fatal("old generation retained")
	}
}

func TestRestartReconcilesCrashDebris(t *testing.T) {
	s, state, root := openStore(t, 1000)
	entry := publish(t, s, "valid", 100)
	missing := publish(t, s, "missing", 100)
	if err := os.RemoveAll(missing.Path); err != nil {
		t.Fatal(err)
	}

	r, err := s.Reserve(100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Path, "body"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(root, "git", "orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(true); err != nil {
		t.Fatal(err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(state, root, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()

	_, release, ok, err := reopened.Acquire(entry.Key)
	if err != nil || !ok {
		t.Fatalf("valid entry lost: %v", err)
	}
	release()

	_, release, ok, err = reopened.Acquire(missing.Key)
	if err != nil || ok {
		t.Fatal("missing entry retained")
	}
	release()

	for _, path := range []string{r.Path, orphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("debris retained: %s", path)
		}
	}

	stats, err := reopened.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Git != 100 || stats.Hits != 1 {
		t.Fatal(stats)
	}
}

func TestSymlinkDirectoryRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "artifacts")); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(t.TempDir(), root, 1000); err == nil {
		t.Fatal("followed a cache-directory symlink")
	}
}
