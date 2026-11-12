package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spedon/lcs/internal/cert"
	"github.com/spedon/lcs/internal/config"
	"github.com/spedon/lcs/internal/proxy"
	"github.com/spedon/lcs/internal/storage"
	toolchain "github.com/spedon/lcs/internal/targets/chip-google-toolchain"
	"github.com/spedon/lcs/internal/targets/git"
)

func Serve(ctx context.Context, c config.Config, paths config.Paths) error {
	var err error
	for _, path := range []string{paths.State, paths.Cache} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%s must be a private directory (mode 0700)", path)
		}
	}
	paths.State, err = filepath.EvalSymlinks(paths.State)
	if err != nil {
		return err
	}
	paths.Cache, err = filepath.EvalSymlinks(paths.Cache)
	if err != nil {
		return err
	}
	if paths.State == paths.Cache || strings.HasPrefix(paths.State, paths.Cache+string(os.PathSeparator)) || strings.HasPrefix(paths.Cache, paths.State+string(os.PathSeparator)) {
		return errors.New("state and cache directories must not overlap")
	}
	var locks []*os.File
	defer func() {
		for _, file := range locks {
			if err := file.Close(); err != nil {
				log.Printf("close service lock: %v", err)
			}
		}
	}()
	for _, dir := range []string{paths.State, paths.Cache} {
		file, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		locks = append(locks, file)
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fmt.Errorf("another service owns %s: %w", dir, err)
		}
	}
	ca, err := cert.Ensure(paths.State)
	if err != nil {
		return err
	}
	store, err := storage.Open(paths.State, paths.Cache, c.Limit)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("close metadata: %v", err)
		}
	}()
	p := proxy.New(ca)
	defer p.Close()
	if c.Toolchain {
		artifacts := toolchain.New(store, p.Transport)
		p.Artifacts = artifacts
		p.Intercept = artifacts.Preset.Intercept
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return err
	}
	defer closeListener(listener)
	socket := filepath.Join(paths.State, "control.sock")
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	control, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer closeListener(control)
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	api := http.NewServeMux()
	api.HandleFunc("GET /shell", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(c); err != nil {
			log.Printf("shell configuration response: %v", err)
		}
	})
	api.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		stats, err := store.Stats()
		if err != nil {
			http.Error(w, "metadata unavailable", 500)
			log.Printf("status: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(stats); err != nil {
			log.Printf("status response: %v", err)
		}
	})
	api.HandleFunc("POST /clear", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Clear(); err != nil {
			http.Error(w, "cache clear failed", 500)
			log.Printf("clear: %v", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	gitcache, err := git.New(c, store, p)
	if err != nil {
		return err
	}
	publicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() && strings.HasPrefix(r.URL.Path, "/git/") && r.Method != http.MethodConnect {
			if r.Host != config.Listen && r.Host != "localhost:7777" {
				http.Error(w, "invalid local host", http.StatusBadRequest)
				return
			}
			gitcache.ServeHTTP(w, r)
			return
		}
		p.ServeHTTP(w, r)
	})
	public := &http.Server{Handler: publicHandler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	private := &http.Server{Handler: api, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	failures := make(chan error, 2)
	go func() { failures <- public.Serve(listener) }()
	go func() { failures <- private.Serve(control) }()
	log.Printf("listening on %s", config.Listen)
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.Close()
	shutdownErr := errors.Join(public.Shutdown(shutdown), private.Shutdown(shutdown))
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, public.Close(), private.Close())
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, shutdownErr)
}

func closeListener(listener net.Listener) {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("close listener: %v", err)
	}
}

func controlClient(paths config.Paths) *http.Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(paths.State, "control.sock"))
	}}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func ShellConfig(ctx context.Context, paths config.Paths) (config.Config, error) {
	var c config.Config
	client := controlClient(paths)
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://lcs/shell", nil)
	if err != nil {
		return c, err
	}
	response, err := client.Do(request)
	if err != nil {
		return c, fmt.Errorf("service is not available: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.Printf("close shell configuration response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK {
		return c, fmt.Errorf("service returned %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(&c); err != nil {
		return c, fmt.Errorf("read daemon configuration: %w", err)
	}
	return c, nil
}

func Control(ctx context.Context, paths config.Paths, action string, output io.Writer) error {
	client := controlClient(paths)
	defer client.CloseIdleConnections()
	method := http.MethodGet
	if action == "clear" {
		method = http.MethodPost
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://lcs/"+action, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		if action == "status" {
			_, writeErr := fmt.Fprintln(output, "LCS\nService: stopped")
			return writeErr
		}
		return fmt.Errorf("service is not available: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.Printf("close control response: %v", err)
		}
	}()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("service returned %s", response.Status)
	}
	if action == "clear" {
		_, err := fmt.Fprintln(output, "Cache cleared; active entries will be removed when released.")
		return err
	}
	var stats storage.Stats
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "LCS\nService: running\nCache: %.1f GB / %.1f GB\n\nGit: %.1f GB\nGoogle toolchain: %.1f GB\nTemporary: %.1f GB\n\nHits: %d\nMisses: %d\nTraffic saved: %.1f GB\n", float64(stats.Git+stats.Toolchain+stats.Temporary)/1e9, float64(stats.Limit)/1e9, float64(stats.Git)/1e9, float64(stats.Toolchain)/1e9, float64(stats.Temporary)/1e9, stats.Hits, stats.Misses, float64(stats.Saved)/1e9)
	return err
}
