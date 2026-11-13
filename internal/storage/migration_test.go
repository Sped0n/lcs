package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeKeepsLegacyCacheUntilClear(t *testing.T) {
	s, state, root := openStore(t, 1000)
	publish(t, s, "repo", 50)

	legacy := filepath.Join(root, "artifacts", "old-generation")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "body"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Exec("INSERT INTO entries(key,kind,path,size,accessed,validation,active) VALUES(?,?,?,?,?,?,?)", "old-artifact", "artifacts", legacy, 100, 1, "{}", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE counters SET hits=7,misses=3,saved=42 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(state, root, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	}()

	stats, err := restarted.Stats()
	if err != nil || stats.Git != 50 || stats.Legacy != 100 || stats.Hits != 7 || stats.Misses != 3 {
		t.Fatalf("upgrade lost cache accounting or counters: %+v, %v", stats, err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "body")); err != nil {
		t.Fatal("upgrade removed published legacy data:", err)
	}

	if err := restarted.Clear(); err != nil {
		t.Fatal(err)
	}
	stats, err = restarted.Stats()
	if err != nil || stats.Git != 0 || stats.Legacy != 0 || stats.Hits != 7 || stats.Misses != 3 {
		t.Fatalf("clear retained cache data or reset counters: %+v, %v", stats, err)
	}

	var saved int64
	if err := restarted.db.QueryRow("SELECT saved FROM counters WHERE id=1").Scan(&saved); err != nil || saved != 42 {
		t.Fatalf("upgrade rewrote historical service state: %d, %v", saved, err)
	}
}
