package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spedon/lcs/internal/storage"
)

const maxRefreshAttempts = 3

func shallowKey(remote string, wants []string) string {
	digest := sha256.Sum256([]byte(strings.Join(wants, "\n")))
	return "git-shallow:" + remote + ":" + hex.EncodeToString(digest[:])
}

func (c *Cache) shallowTags(ctx context.Context, remote string, wants []string) (map[string]string, error) {
	output, err := c.command(ctx, "", "ls-remote", "--tags", remote)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validObjectID(fields[0]) || !strings.HasPrefix(fields[1], "refs/tags/") {
			return nil, errors.New("invalid Git tag advertisement")
		}
		refs[fields[1]] = fields[0]
	}

	tags := map[string]string{}
	for name, oid := range refs {
		if !strings.HasSuffix(name, "^{}") && (slices.Contains(wants, oid) || slices.Contains(wants, refs[name+"^{}"])) {
			tags[name] = oid
		}
	}
	if len(tags) > 32 {
		return nil, errors.New("too many tags for a shallow cache entry")
	}

	return tags, nil
}

func (c *Cache) prepareShallow(ctx context.Context, remote, key string, wants []string, tags map[string]string) error {
	if err := c.store.Record(false); err != nil {
		return err
	}
	reservation, release, err := c.reserveRepository(ctx)
	if err != nil {
		return err
	}
	defer release()
	log.Printf("Git cache fill %s: depth 1, %d objects", remote, len(wants))

	stage := filepath.Join(reservation.Path, "repo.git")
	err = c.stageRepository(ctx, reservation, func(ctx context.Context) error {
		format := "sha1"
		if len(wants[0]) == 64 {
			format = "sha256"
		}
		if _, err := c.command(ctx, "", "init", "--bare", "--object-format="+format, stage); err != nil {
			return err
		}

		args := append([]string{"fetch", "--depth=1", "--force", "--no-auto-maintenance", "--", remote}, wants...)
		if _, err := c.command(ctx, stage, args...); err != nil {
			return err
		}

		var refs []string
		for _, oid := range wants {
			refs = append(refs, "update refs/lcs/"+oid+" "+oid)
		}
		for name, oid := range tags {
			refs = append(refs, "update "+name+" "+oid)
		}

		_, err := c.commandInput(ctx, stage, strings.NewReader(strings.Join(refs, "\n")+"\n"), "update-ref", "--stdin")
		return err
	})
	if err != nil {
		return err
	}

	if err := reservation.Publish(key, ""); err != nil {
		return err
	}

	log.Printf("Git cache ready %s: depth 1", remote)
	return nil
}

func (c *Cache) prepare(ctx context.Context, remote string, wants []string) error {
	entry, release, found, err := c.store.Acquire("git:" + remote)
	if err != nil {
		return err
	}
	defer release()

	if found {
		refs, err := c.command(ctx, "", "ls-remote", "--symref", remote)
		if err != nil {
			return err
		}
		upstream, err := parseRefSnapshot(refs)
		if err != nil {
			return err
		}
		cached, err := c.localSnapshot(ctx, filepath.Join(entry.Path, "repo.git"))
		if err != nil {
			return err
		}
		if cached.fingerprint == upstream.fingerprint {
			_, releaseCheck, present, err := c.acquire(ctx, "git:"+remote, wants)
			releaseCheck()
			if err != nil {
				return err
			}
			if !present {
				return errors.New("requested objects are not in the mirror")
			}
			return c.store.Record(true)
		}
	}
	if err := c.store.Record(false); err != nil {
		return err
	}

	reservation, releaseReservation, err := c.reserveRepository(ctx)
	if err != nil {
		return err
	}
	defer releaseReservation()
	log.Printf("Git cache fill %s: mirror", remote)

	stage := filepath.Join(reservation.Path, "repo.git")
	var staged refSnapshot
	err = c.stageRepository(ctx, reservation, func(ctx context.Context) error {
		if found {
			if err := reservation.Grow(entry.Size); err != nil {
				return err
			}
			if err := copyRepository(ctx, filepath.Join(entry.Path, "repo.git"), stage); err != nil {
				return err
			}
		} else if _, err := c.command(ctx, "", "clone", "--mirror", "--", remote, stage); err != nil {
			return err
		}

		for attempt := 0; ; attempt++ {
			if found || attempt > 0 {
				if _, err := c.command(ctx, stage, "fetch", "--prune", "--force", "--no-auto-maintenance", "origin", "+refs/*:refs/*", "HEAD"); err != nil {
					return err
				}
			}

			refs, err := c.command(ctx, "", "ls-remote", "--symref", remote)
			if err != nil {
				return err
			}

			upstream, err := parseRefSnapshot(refs)
			if err != nil {
				return err
			}
			if upstream.headRef != "" {
				_, err = c.command(ctx, stage, "symbolic-ref", "HEAD", upstream.headRef)
			} else {
				_, err = c.command(ctx, stage, "update-ref", "--no-deref", "HEAD", upstream.headOID)
			}
			if err != nil {
				return err
			}

			staged, err = c.localSnapshot(ctx, stage)
			if err != nil {
				return err
			}
			if staged.fingerprint == upstream.fingerprint {
				return nil
			}

			mismatch := refMismatch(upstream, staged)
			if attempt+1 == maxRefreshAttempts {
				return mismatch
			}
			log.Printf("Git cache reconcile %s (%d/%d): %v", remote, attempt+1, maxRefreshAttempts, mismatch)
		}
	})
	if err != nil {
		return err
	}

	release()
	if err := reservation.Publish("git:"+remote, staged.fingerprint); err != nil {
		return err
	}
	log.Printf("Git cache ready %s: mirror", remote)
	return nil
}

func (c *Cache) reserveRepository(ctx context.Context) (*storage.Reservation, func(), error) {
	select {
	case c.downloads <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	reservation, err := c.store.Reserve(0)
	if err != nil {
		<-c.downloads
		return nil, nil, err
	}
	release := func() {
		if err := reservation.Abort(); err != nil {
			log.Printf("remove Git staging: %v", err)
		}
		<-c.downloads
	}

	return reservation, release, nil
}

func (c *Cache) stageRepository(ctx context.Context, reservation *storage.Reservation, populate func(context.Context) error) error {
	// Sampling limits logical staging growth, but cannot enforce a filesystem quota.
	download, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	checked := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				checked <- nil
				return
			case <-download.Done():
				checked <- download.Err()
				return
			case <-ticker.C:
				size, err := storage.DiskUsage(reservation.Path)
				if err == nil {
					err = reservation.Grow(size)
				}
				if err != nil {
					cancel()
					checked <- err
					return
				}
			}
		}
	}()

	err := populate(download)
	stage := filepath.Join(reservation.Path, "repo.git")
	if err == nil {
		_, err = c.command(download, stage, "fsck", "--connectivity-only")
	}

	for _, setting := range [][2]string{{"uploadpack.allowFilter", "true"}, {"http.receivepack", "false"}, {"uploadpack.allowReachableSHA1InWant", "true"}} {
		if err != nil {
			break
		}
		_, err = c.command(download, stage, "config", setting[0], setting[1])
	}

	close(done)
	return errors.Join(err, <-checked)
}

func copyRepository(ctx context.Context, source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected Git repository file type")
		}

		// Link immutable objects only. Copy refs, config, shallow boundaries and other metadata.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) == 3 && parts[0] == "objects" {
			immutable := len(parts[1]) == 2 && validObjectID(parts[1]+parts[2])
			if parts[1] == "pack" {
				ext := filepath.Ext(parts[2])
				immutable = ext == ".pack" || ext == ".idx" || ext == ".rev"
			}
			if immutable && os.Link(path, target) == nil {
				return nil
			}
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.Join(err, in.Close())
		}

		_, copyErr := io.Copy(out, in)
		return errors.Join(copyErr, out.Close(), in.Close())
	})
}
