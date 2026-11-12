package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrCapacity = errors.New("cache has no available space")

type Entry struct {
	Key, Kind, Path, Validation string
	Size                        int64
}

type Stats struct {
	Limit, Git, Toolchain, Temporary, Hits, Misses, Saved int64
}

type Store struct {
	mu              sync.Mutex
	db              *sql.DB
	root            string
	limit, reserved int64
	active          map[string]int
	pending         map[string]bool
	epoch           uint64
}

func Open(state, root string, limit int64) (*Store, error) {
	for _, name := range []string{"git", "artifacts", "tmp"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s must be a private directory", path)
		}
	}
	file, err := os.OpenFile(filepath.Join(state, "metadata.db"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(state, "metadata.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, root: root, limit: limit, active: map[string]int{}, pending: map[string]bool{}}
	if err := s.initialize(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

func (s *Store) initialize() error {
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS entries (key TEXT PRIMARY KEY, kind TEXT NOT NULL, path TEXT UNIQUE NOT NULL, size INTEGER NOT NULL, accessed INTEGER NOT NULL, validation TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS counters (id INTEGER PRIMARY KEY CHECK(id=1), hits INTEGER NOT NULL, misses INTEGER NOT NULL, saved INTEGER NOT NULL);
INSERT OR IGNORE INTO counters VALUES(1,0,0,0);
UPDATE entries SET active=0;`)
	if err != nil {
		return err
	}
	rows, err := s.db.Query("SELECT key,path FROM entries")
	if err != nil {
		return err
	}
	known := map[string]bool{}
	var missing []string
	for rows.Next() {
		var key, path string
		if err := rows.Scan(&key, &path); err != nil {
			return errors.Join(err, rows.Close())
		}
		if !s.validPath(path) {
			return errors.Join(errors.New("metadata contains a path outside the cache"), rows.Close())
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, key)
		} else if err != nil {
			return errors.Join(err, rows.Close())
		} else {
			known[path] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, key := range missing {
		if _, err := s.db.Exec("DELETE FROM entries WHERE key=?", key); err != nil {
			return err
		}
	}
	for path := range known {
		size, err := DiskUsage(path)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec("UPDATE entries SET size=? WHERE path=?", size, path); err != nil {
			return err
		}
	}
	for _, kind := range []string{"git", "artifacts", "tmp"} {
		dir := filepath.Join(s.root, kind)
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, file := range files {
			path := filepath.Join(dir, file.Name())
			if !known[path] {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
		}
	}
	return s.evictLocked(0, false)
}

func (s *Store) validPath(path string) bool {
	return filepath.Dir(filepath.Dir(path)) == s.root && (filepath.Base(filepath.Dir(path)) == "git" || filepath.Base(filepath.Dir(path)) == "artifacts")
}

func (s *Store) Close() error { return s.db.Close() }

// Acquire pins an immutable generation until release. A replacement never changes it.
func (s *Store) Acquire(key string) (Entry, func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var e Entry
	err := s.db.QueryRow("SELECT key,kind,path,size,validation FROM entries WHERE key=?", key).Scan(&e.Key, &e.Kind, &e.Path, &e.Size, &e.Validation)
	if errors.Is(err, sql.ErrNoRows) {
		return e, func() {}, false, nil
	}
	if err != nil {
		return e, nil, false, err
	}
	if s.pending[e.Path] {
		return Entry{}, func() {}, false, nil
	}
	s.active[e.Path]++
	if _, err := s.db.Exec("UPDATE entries SET accessed=?,active=active+1 WHERE key=?", time.Now().UnixNano(), key); err != nil {
		s.active[e.Path]--
		return Entry{}, nil, false, err
	}
	var once sync.Once
	return e, func() { once.Do(func() { s.release(e.Path) }) }, true, nil
}

func (s *Store) release(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[path]--
	if s.active[path] == 0 {
		delete(s.active, path)
	}
	if _, err := s.db.Exec("UPDATE entries SET active=MAX(0,active-1) WHERE path=?", path); err != nil {
		log.Printf("release cache entry: %v", err)
		s.pending[path] = true
		return
	}
	if s.active[path] == 0 && s.pending[path] {
		// Failure leaves a tracked entry for a later clear or startup recovery.
		if err := s.removeLocked(path); err == nil {
			delete(s.pending, path)
		} else {
			log.Printf("remove released cache entry: %v", err)
		}
	}
}

func (s *Store) usageLocked() (int64, error) {
	var size int64
	err := s.db.QueryRow("SELECT COALESCE(SUM(size),0) FROM entries").Scan(&size)
	return size, err
}

func (s *Store) removeLocked(path string) error {
	if !s.validPath(path) {
		return errors.New("refuse to remove an invalid cache path")
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	_, err := s.db.Exec("DELETE FROM entries WHERE path=?", path)
	return err
}

func (s *Store) evictLocked(want int64, clear bool) error {
	used, err := s.usageLocked()
	if err != nil {
		return err
	}
	target := s.limit
	if clear {
		target = 0
	} else if float64(used)+float64(s.reserved)+float64(want) >= float64(s.limit)*0.9 {
		target = s.limit / 10 * 8
	}
	rows, err := s.db.Query("SELECT path,size FROM entries ORDER BY accessed ASC")
	if err != nil {
		return err
	}
	type candidate struct {
		path string
		size int64
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.path, &c.size); err != nil {
			return errors.Join(err, rows.Close())
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, c := range candidates {
		if !clear && float64(used)+float64(s.reserved)+float64(want) <= float64(target) {
			break
		}
		if s.active[c.path] != 0 {
			if clear {
				s.pending[c.path] = true
			}
			continue
		}
		if err := s.removeLocked(c.path); err != nil {
			return err
		}
		delete(s.pending, c.path)
		used -= c.size
	}
	if want > s.limit-used-s.reserved {
		return ErrCapacity
	}
	return nil
}

type Reservation struct {
	store *Store
	Path  string
	Limit int64
	done  bool
	epoch uint64
}

// Reserve holds capacity for staging, including replacement generations.
// A zero request takes all remaining capacity for a Git download of unknown size.
func (s *Store) Reserve(size int64) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if size < 0 || size > s.limit {
		return nil, ErrCapacity
	}
	if err := s.evictLocked(size, false); err != nil {
		return nil, err
	}
	used, err := s.usageLocked()
	if err != nil {
		return nil, err
	}
	if size == 0 {
		size = s.limit - used - s.reserved
	}
	if size <= 0 || size > s.limit-used-s.reserved {
		return nil, ErrCapacity
	}
	path, err := os.MkdirTemp(filepath.Join(s.root, "tmp"), "stage-")
	if err != nil {
		return nil, err
	}
	s.reserved += size
	return &Reservation{store: s, Path: path, Limit: size, epoch: s.epoch}, nil
}

func (r *Reservation) Abort() error {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.done {
		return nil
	}
	if err := os.RemoveAll(r.Path); err != nil {
		return err
	}
	s.reserved -= r.Limit
	r.done = true
	return nil
}

func (r *Reservation) Publish(key, kind, validation string) error {
	size, err := DiskUsage(r.Path)
	if err != nil {
		return err
	}
	if size > r.Limit {
		return ErrCapacity
	}
	if err := syncTree(r.Path); err != nil {
		return err
	}
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.done {
		return errors.New("reservation is already closed")
	}
	if r.epoch != s.epoch {
		return errors.New("cache was cleared during download")
	}
	if kind != "git" && kind != "artifacts" {
		return errors.New("invalid resource kind")
	}
	var old string
	err = s.db.QueryRow("SELECT path FROM entries WHERE key=?", key).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if old != "" && s.active[old] != 0 {
		return errors.New("cannot replace an active cache generation")
	}
	if old != "" {
		if err := s.removeLocked(old); err != nil {
			return err
		}
	}
	destination := filepath.Join(s.root, kind, filepath.Base(r.Path))
	if err := os.Rename(r.Path, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return errors.Join(err, os.Rename(destination, r.Path))
	}
	if _, err := s.db.Exec("INSERT INTO entries(key,kind,path,size,accessed,validation) VALUES(?,?,?,?,?,?) ON CONFLICT(key) DO UPDATE SET kind=excluded.kind,path=excluded.path,size=excluded.size,accessed=excluded.accessed,validation=excluded.validation,active=0", key, kind, destination, size, time.Now().UnixNano(), validation); err != nil {
		if removeErr := os.RemoveAll(destination); removeErr != nil {
			return errors.Join(err, removeErr)
		}
		return err
	}
	r.done = true
	s.reserved -= r.Limit
	return nil
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncTree(path string) error {
	return filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not allowed in cache entries")
		}
		return syncDirectory(path)
	})
}

func DiskUsage(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && current != path {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlinks are not allowed in cache entries")
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("unsupported cache file type")
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	return s.evictLocked(0, true)
}

func (s *Store) Invalidate(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var path string
	if err := s.db.QueryRow("SELECT path FROM entries WHERE key=?", key).Scan(&path); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if s.active[path] != 0 {
		s.pending[path] = true
		return nil
	}
	return s.removeLocked(path)
}

func (s *Store) UpdateValidation(key, validation string) error {
	_, err := s.db.Exec("UPDATE entries SET validation=? WHERE key=?", validation, key)
	return err
}

func (s *Store) Record(hit bool, saved int64) error {
	hits, misses := 0, 1
	if hit {
		hits, misses = 1, 0
	}
	_, err := s.db.Exec("UPDATE counters SET hits=hits+?,misses=misses+?,saved=saved+? WHERE id=1", hits, misses, saved)
	return err
}

func (s *Store) Stats() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := Stats{Limit: s.limit}
	if err := s.db.QueryRow("SELECT COALESCE(SUM(CASE WHEN kind='git' THEN size ELSE 0 END),0),COALESCE(SUM(CASE WHEN kind='artifacts' THEN size ELSE 0 END),0) FROM entries").Scan(&stats.Git, &stats.Toolchain); err != nil {
		return stats, err
	}
	if err := s.db.QueryRow("SELECT hits,misses,saved FROM counters WHERE id=1").Scan(&stats.Hits, &stats.Misses, &stats.Saved); err != nil {
		return stats, err
	}
	size, err := DiskUsage(filepath.Join(s.root, "tmp"))
	stats.Temporary = size
	return stats, err
}
