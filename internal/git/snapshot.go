package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type refSnapshot struct {
	fingerprint string
	headRef     string
	headOID     string
	refs        map[string]string
}

func validObjectID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

func parseRefSnapshot(data []byte) (refSnapshot, error) {
	snapshot := refSnapshot{refs: map[string]string{}}
	var lines []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		value, name, ok := strings.Cut(line, "\t")
		if !ok {
			return snapshot, errors.New("invalid Git ref advertisement")
		}
		if ref, ok := strings.CutPrefix(value, "ref: "); ok {
			if name == "HEAD" {
				snapshot.headRef = ref
				lines = append(lines, line)
			}
			// A mirror resolves other symbolic refs to object IDs.
			continue
		}
		if !validObjectID(value) {
			return snapshot, errors.New("ref advertisement has an invalid object ID")
		}
		snapshot.refs[name] = value
		lines = append(lines, line)
	}

	snapshot.headOID = snapshot.refs["HEAD"]
	if snapshot.headOID == "" {
		return snapshot, errors.New("ref advertisement has no valid HEAD object")
	}

	sort.Strings(lines)
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	snapshot.fingerprint = hex.EncodeToString(digest[:])
	return snapshot, nil
}

func refMismatch(upstream, staged refSnapshot) error {
	var differences []string
	if upstream.headRef != staged.headRef {
		differences = append(differences, fmt.Sprintf("HEAD target staged=%q upstream=%q", staged.headRef, upstream.headRef))
	}

	names := map[string]bool{}
	for name := range upstream.refs {
		names[name] = true
	}
	for name := range staged.refs {
		names[name] = true
	}

	var sorted []string
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	count := len(differences)
	for _, name := range sorted {
		if upstream.refs[name] == staged.refs[name] {
			continue
		}
		count++
		if len(differences) < 5 {
			differences = append(differences, fmt.Sprintf("%.160s staged=%q upstream=%q", name, staged.refs[name], upstream.refs[name]))
		}
	}

	return fmt.Errorf("upstream and staged refs differ (%d differences): %s", count, strings.Join(differences, "; "))
}

func (c *Cache) localSnapshot(ctx context.Context, path string) (refSnapshot, error) {
	// File transport is allowed only for this cache-owned repository, never upstream URLs.
	refs, err := c.command(ctx, path, "-c", "protocol.file.allow=always", "ls-remote", "--symref", ".")
	if err != nil {
		return refSnapshot{}, err
	}

	return parseRefSnapshot(refs)
}
