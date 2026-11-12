package config

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

const Listen = "127.0.0.1:7777"
const Endpoint = "http://" + Listen

type Config struct {
	CacheSize    string   `toml:"cache_size"`
	Cache        []string `toml:"cache"`
	Limit        int64    `toml:"-"`
	Repositories []string `toml:"-"`
	Toolchain    bool     `toml:"-"`
}

type Paths struct{ Config, State, Cache string }

func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	resolve := func(systemd, xdg, fallback string) (string, error) {
		if p := os.Getenv(systemd); systemd != "" && p != "" {
			if strings.Contains(p, ":") || !filepath.IsAbs(p) {
				return "", fmt.Errorf("%s must name one absolute directory", systemd)
			}
			return filepath.Clean(p), nil
		}
		base := os.Getenv(xdg)
		if base == "" {
			base = filepath.Join(home, fallback)
		}
		if !filepath.IsAbs(base) {
			return "", fmt.Errorf("%s must be absolute", xdg)
		}
		return filepath.Join(base, "lcs"), nil
	}
	conf, err := resolve("", "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Paths{}, err
	}
	state, err := resolve("STATE_DIRECTORY", "XDG_STATE_HOME", ".local/state")
	if err != nil {
		return Paths{}, err
	}
	cache, err := resolve("CACHE_DIRECTORY", "XDG_CACHE_HOME", ".cache")
	if err != nil {
		return Paths{}, err
	}
	if state == cache || strings.HasPrefix(state, cache+string(os.PathSeparator)) || strings.HasPrefix(cache, state+string(os.PathSeparator)) {
		return Paths{}, errors.New("state and cache directories must not overlap")
	}
	return Paths{Config: filepath.Join(conf, "config.toml"), State: state, Cache: cache}, nil
}

func Load(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	metadata, err := toml.Decode(string(data), &c)
	if err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if unknown := metadata.Undecoded(); len(unknown) != 0 {
		return c, fmt.Errorf("unknown config keys: %v", unknown)
	}
	c.Limit, err = ParseSize(c.CacheSize)
	if err != nil {
		return c, err
	}
	seen := make(map[string]bool)
	for _, entry := range c.Cache {
		var remote string
		switch {
		case entry == "chip-google-toolchain":
			c.Toolchain = true
			continue
		case strings.HasPrefix(entry, "git:"):
			remote, err = RepositoryURL(strings.TrimPrefix(entry, "git:"))
			if err != nil {
				return c, fmt.Errorf("cache %q: %w", entry, err)
			}
		default:
			return c, fmt.Errorf("unknown cache entry %q", entry)
		}
		if !seen[remote] {
			c.Repositories = append(c.Repositories, remote)
			seen[remote] = true
		}
	}
	return c, nil
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(B|KB|MB|GB|TB)$`)

func ParseSize(value string) (int64, error) {
	parts := sizePattern.FindStringSubmatch(value)
	if parts == nil {
		return 0, errors.New("cache_size must use decimal units, for example 50GB")
	}
	number, ok := new(big.Rat).SetString(parts[1])
	if !ok {
		return 0, errors.New("invalid cache_size")
	}
	multiplier := map[string]int64{"B": 1, "KB": 1000, "MB": 1000000, "GB": 1000000000, "TB": 1000000000000}[parts[2]]
	bytes := new(big.Rat).Mul(number, new(big.Rat).SetInt64(multiplier))
	if !bytes.IsInt() || !bytes.Num().IsInt64() || bytes.Sign() <= 0 {
		return 0, errors.New("cache_size must be a positive whole byte count within int64")
	}
	return bytes.Num().Int64(), nil
}

// RepositoryURL accepts credential-free HTTPS remotes with unambiguous paths.
func RepositoryURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("repository must be a public HTTPS URL without credentials, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || !strings.Contains(host, ".") || net.ParseIP(host) != nil || u.Port() != "" && u.Port() != "443" {
		return "", errors.New("repository must use a public DNS name and HTTPS port 443")
	}
	if strings.ContainsAny(raw, "\\\r\n\t ") || strings.Contains(u.EscapedPath(), "%") {
		return "", errors.New("repository URL contains unsafe characters")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", errors.New("repository URL must include owner and repository")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
			return "", errors.New("repository URL contains an unsafe path component")
		}
	}
	u.Host = host
	u.Path = "/" + strings.Join(parts, "/")
	return u.String(), nil
}
