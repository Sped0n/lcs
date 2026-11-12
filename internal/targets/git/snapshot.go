package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

type refSnapshot struct {
	fingerprint string
	headRef     string
	headOID     string
}

func parseRefSnapshot(data []byte) (refSnapshot, error) {
	var snapshot refSnapshot
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	sort.Strings(lines)
	for _, line := range lines {
		value, name, ok := strings.Cut(line, "\t")
		if !ok {
			return snapshot, errors.New("invalid Git ref advertisement")
		}
		if name != "HEAD" {
			continue
		}
		if ref, ok := strings.CutPrefix(value, "ref: "); ok {
			snapshot.headRef = ref
		} else {
			snapshot.headOID = value
		}
	}
	if len(snapshot.headOID) != 40 && len(snapshot.headOID) != 64 {
		return snapshot, errors.New("ref advertisement has no valid HEAD object")
	}
	if _, err := hex.DecodeString(snapshot.headOID); err != nil {
		return snapshot, errors.New("ref advertisement has an invalid HEAD object")
	}
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	snapshot.fingerprint = hex.EncodeToString(digest[:])
	return snapshot, nil
}

func (c *Cache) localSnapshot(ctx context.Context, path string) (refSnapshot, error) {
	// File transport is allowed only for this cache-owned repository, never upstream URLs.
	refs, err := c.command(ctx, path, "-c", "protocol.file.allow=always", "ls-remote", "--symref", ".")
	if err != nil {
		return refSnapshot{}, err
	}
	return parseRefSnapshot(refs)
}
