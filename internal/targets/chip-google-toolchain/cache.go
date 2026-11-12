package toolchain

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spedon/lcs/internal/storage"
)

type Cache struct {
	Upstream http.RoundTripper
	Preset   *Preset
	store    *storage.Store
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

func New(store *storage.Store, upstream http.RoundTripper) *Cache {
	return &Cache{Upstream: upstream, Preset: NewPreset(), store: store, inflight: map[string]chan struct{}{}}
}

type metadata struct {
	Header     http.Header
	Received   time.Time
	InitialAge float64
	Hash       string
	Size       int64
}

func forbidden(header http.Header) bool {
	for _, value := range strings.Split(strings.ToLower(strings.Join(header.Values("Cache-Control"), ",")), ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(value), "=")
		if name == "no-store" || name == "private" {
			return true
		}
	}
	return false
}

func (c *Cache) RoundTrip(r *http.Request) (*http.Response, error) {
	identity, eligible := c.Preset.Identity(r.URL)
	if !eligible || r.Method != http.MethodGet || len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values("Cookie")) != 0 || r.Header.Get("Cache-Control") != "" || r.Header.Get("Pragma") != "" {
		return c.Upstream.RoundTrip(r)
	}
	for _, name := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		if r.Header.Get(name) != "" {
			return c.Upstream.RoundTrip(r)
		}
	}
	// Hash all end-to-end request headers: arbitrary Vary fields cannot cross variants.
	var requestKey strings.Builder
	requestKey.WriteString(identity + "\n" + r.URL.String() + "\n")
	keys := make([]string, 0, len(r.Header))
	for key := range r.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&requestKey, "%s:%q\n", key, r.Header.Values(key))
	}
	digest := sha256.Sum256([]byte(requestKey.String()))
	key := "artifact:" + hex.EncodeToString(digest[:])
	for {
		c.mu.Lock()
		wait, busy := c.inflight[key]
		if !busy {
			c.inflight[key] = make(chan struct{})
			c.mu.Unlock()
			break
		}
		c.mu.Unlock()
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-wait:
		}
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() { c.mu.Lock(); close(c.inflight[key]); delete(c.inflight, key); c.mu.Unlock() })
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	entry, release, found, err := c.store.Acquire(key)
	if err != nil {
		log.Printf("artifact metadata unavailable: %v", err)
		return c.Upstream.RoundTrip(r)
	}
	defer func() { release() }()
	var saved metadata
	if found {
		decodeErr := json.Unmarshal([]byte(entry.Validation), &saved)
		if decodeErr == nil {
			decodeErr = verifyFile(entry.Path, saved)
		}
		if err := decodeErr; err != nil {
			log.Printf("discard corrupt artifact: %v", err)
			if err := c.store.Invalidate(key); err != nil {
				return nil, err
			}
			release()
			found = false
		}
	}
	if found && fresh(saved, r) {
		response, err := cachedResponse(r, entry.Path, saved)
		if err != nil {
			return nil, err
		}
		response.Body = &cachedBody{ReadCloser: response.Body, release: release, store: c.store}
		release = func() {}
		return response, nil
	}
	out := r.Clone(r.Context())
	if found {
		if value := saved.Header.Get("ETag"); value != "" {
			out.Header.Set("If-None-Match", value)
		} else if value := saved.Header.Get("Last-Modified"); value != "" {
			out.Header.Set("If-Modified-Since", value)
		}
	}
	started := time.Now()
	response, err := c.Upstream.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	if found && response.StatusCode == http.StatusNotModified {
		closeResponse(response.Body)
		for _, name := range []string{"Date", "Age", "Cache-Control", "ETag", "Expires", "Last-Modified", "Vary"} {
			if values, ok := response.Header[name]; ok {
				saved.Header[name] = values
			}
		}
		saved.Received = time.Now()
		saved.InitialAge = initialAge(saved.Header, started, saved.Received)
		encoded, err := json.Marshal(saved)
		if err != nil {
			return nil, err
		}
		if forbidden(saved.Header) || len(response.Header.Values("Set-Cookie")) != 0 || strings.Contains(strings.Join(saved.Header.Values("Vary"), ","), "*") {
			if err := c.store.Invalidate(key); err != nil {
				return nil, err
			}
			// Retry with the original request when validation changes cache eligibility.
			return c.Upstream.RoundTrip(r)
		}
		if err := c.store.UpdateValidation(key, string(encoded)); err != nil {
			return nil, err
		}
		response, err = cachedResponse(r, entry.Path, saved)
		if err != nil {
			return nil, err
		}
		response.Body = &cachedBody{ReadCloser: response.Body, release: release, store: c.store}
		release = func() {}
		return response, nil
	}
	if found {
		if err := c.store.Invalidate(key); err != nil {
			log.Printf("invalidate artifact: %v", err)
			return response, nil
		}
		release()
	}
	c.Preset.Learn(r, response)
	if err := c.store.Record(false, 0); err != nil {
		closeResponse(response.Body)
		return nil, err
	}
	if response.StatusCode != http.StatusOK || response.ContentLength <= 0 || forbidden(response.Header) || len(response.Header.Values("Set-Cookie")) != 0 || strings.Contains(strings.Join(response.Header.Values("Vary"), ","), "*") || response.Header.Get("Content-Range") != "" {
		return response, nil
	}
	release()
	reservation, err := c.store.Reserve(response.ContentLength)
	if errors.Is(err, storage.ErrCapacity) {
		return response, nil
	}
	if err != nil {
		closeResponse(response.Body)
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(reservation.Path, "download"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		abortErr := reservation.Abort()
		closeErr := response.Body.Close()
		return nil, errors.Join(err, abortErr, closeErr)
	}
	meta := metadata{Header: response.Header.Clone(), Received: time.Now(), Size: response.ContentLength}
	meta.InitialAge = initialAge(response.Header, started, meta.Received)
	response.Body = &downloadBody{ReadCloser: response.Body, file: file, reservation: reservation, hash: sha256.New(), meta: meta, key: key, unlock: unlock}
	unlock = nil
	return response, nil
}

func initialAge(header http.Header, started, received time.Time) float64 {
	age, _ := strconv.ParseFloat(header.Get("Age"), 64)
	date, err := http.ParseTime(header.Get("Date"))
	apparent := 0.0
	if err == nil {
		apparent = max(0, received.Sub(date).Seconds())
	}
	return max(apparent, max(0, age)+received.Sub(started).Seconds())
}

func fresh(m metadata, r *http.Request) bool {
	requestControl := strings.ToLower(r.Header.Get("Cache-Control"))
	if strings.Contains(requestControl, "no-cache") || strings.Contains(requestControl, "max-age=0") || r.Header.Get("Pragma") == "no-cache" {
		return false
	}
	lifetime := 0.0
	var maxAge, sharedAge *float64
	for _, directive := range strings.Split(strings.ToLower(strings.Join(m.Header.Values("Cache-Control"), ",")), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), "=")
		if name == "no-cache" {
			return false
		}
		if name == "max-age" || name == "s-maxage" {
			parsed, err := strconv.ParseUint(strings.Trim(value, "\""), 10, 63)
			if err != nil {
				return false
			}
			seconds := float64(parsed)
			if name == "max-age" {
				if maxAge != nil {
					return false
				}
				maxAge = &seconds
			} else {
				if sharedAge != nil {
					return false
				}
				sharedAge = &seconds
			}
		}
	}
	if sharedAge != nil {
		lifetime = *sharedAge
	} else if maxAge != nil {
		lifetime = *maxAge
	} else {
		expires, err := http.ParseTime(m.Header.Get("Expires"))
		if err == nil {
			date, err := http.ParseTime(m.Header.Get("Date"))
			if err == nil {
				lifetime = expires.Sub(date).Seconds()
			}
		}
	}
	age := m.InitialAge + time.Since(m.Received).Seconds()
	return age < lifetime
}

func verifyFile(dir string, m metadata) error {
	if len(m.Hash) != 64 || strings.ContainsAny(m.Hash, "/\\") {
		return errors.New("invalid content hash")
	}
	file, err := os.Open(filepath.Join(dir, m.Hash))
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Printf("close cached artifact: %v", err)
		}
	}()
	h := sha256.New()
	size, err := io.Copy(h, file)
	if err != nil {
		return err
	}
	if size != m.Size || hex.EncodeToString(h.Sum(nil)) != m.Hash {
		return errors.New("artifact content hash mismatch")
	}
	return nil
}

func cachedResponse(r *http.Request, dir string, m metadata) (*http.Response, error) {
	file, err := os.Open(filepath.Join(dir, m.Hash))
	if err != nil {
		return nil, err
	}
	header := m.Header.Clone()
	header.Set("Age", strconv.FormatInt(int64(m.InitialAge+time.Since(m.Received).Seconds()), 10))
	return &http.Response{StatusCode: 200, Status: "200 OK", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: header, ContentLength: m.Size, Body: file, Request: r}, nil
}

func closeResponse(body io.Closer) {
	if err := body.Close(); err != nil {
		log.Printf("close artifact response: %v", err)
	}
}

type cachedBody struct {
	io.ReadCloser
	release func()
	store   *storage.Store
	read    atomic.Int64
	once    sync.Once
}

func (b *cachedBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	b.read.Add(int64(n))
	return n, err
}

func (b *cachedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		defer b.release()
		if err := b.store.Record(true, b.read.Load()); err != nil {
			log.Printf("artifact statistics: %v", err)
		}
	})
	return err
}

type downloadBody struct {
	io.ReadCloser
	file             *os.File
	reservation      *storage.Reservation
	hash             hash.Hash
	meta             metadata
	key              string
	written          int64
	failed, finished bool
	unlock           func()
}

func (b *downloadBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if n > 0 && !b.failed && b.written+int64(n) > b.reservation.Limit {
		b.failed = true
	}
	if n > 0 && !b.failed {
		_, writeErr := b.file.Write(data[:n])
		if writeErr != nil {
			b.failed = true
			log.Printf("artifact staging: %v", writeErr)
		} else {
			b.hash.Write(data[:n])
			b.written += int64(n)
		}
	}
	if err == io.EOF {
		b.finish()
	} else if err != nil {
		b.failed = true
	}
	return n, err
}

func (b *downloadBody) finish() {
	if b.finished {
		return
	}
	b.finished = true
	defer b.unlock()
	defer func() {
		if err := b.reservation.Abort(); err != nil {
			log.Printf("artifact cleanup: %v", err)
		}
	}()
	if err := b.file.Sync(); err != nil {
		b.failed = true
		log.Printf("artifact sync: %v", err)
	}
	if err := b.file.Close(); err != nil {
		b.failed = true
		log.Printf("artifact close: %v", err)
	}
	if b.failed || b.written != b.meta.Size {
		return
	}
	sum := b.hash.Sum(nil)
	for _, part := range strings.Split(b.meta.Header.Get("Digest"), ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(name, "sha-256") && value != base64.StdEncoding.EncodeToString(sum) {
			log.Print("artifact digest mismatch")
			return
		}
	}
	b.meta.Hash = hex.EncodeToString(sum)
	if err := os.Rename(filepath.Join(b.reservation.Path, "download"), filepath.Join(b.reservation.Path, b.meta.Hash)); err != nil {
		log.Printf("artifact content address: %v", err)
		return
	}
	encoded, err := json.Marshal(b.meta)
	if err != nil {
		log.Printf("artifact metadata: %v", err)
		return
	}
	if err := b.reservation.Publish(b.key, "artifacts", string(encoded)); err != nil {
		log.Printf("artifact publication: %v", err)
	}
}

func (b *downloadBody) Close() error {
	err := b.ReadCloser.Close()
	if !b.finished {
		b.failed = true
		b.finish()
	}
	return err
}
