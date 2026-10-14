// Package envfile loads $data_dir/env, an optional KEY=VALUE file whose
// variables bosund adds to its own environment at startup. It exists so
// service definitions (launchd plists, systemd units) never contain secrets
// such as API keys: they live in one mode-0600 file next to the vault.
package envfile

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// MaxSize bounds the env file.
const MaxSize = 64 << 10

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse parses KEY=VALUE lines. Blank lines and lines starting with # are
// skipped; an optional leading "export " is allowed; a value wrapped in
// matching single or double quotes is unquoted (no escape processing).
func Parse(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), MaxSize)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !keyRe.MatchString(k) {
			return nil, fmt.Errorf("line %d: want KEY=VALUE", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Load reads path and sets each variable that is not already set in the
// process environment (an explicit environment wins over the file). A
// missing file is not an error. The file must be a regular file owned by
// the current user with no group or other permissions (0600 or stricter),
// otherwise it is refused. It returns the names it set (never the values).
func Load(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("env file %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("env file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("env file %s: not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("env file %s has mode %04o; it may hold secrets, so it must be 0600 (run `chmod 600 %s`)", path, perm, path)
	}
	if err := checkOwner(fi); err != nil {
		return nil, fmt.Errorf("env file %s: %w", path, err)
	}
	if fi.Size() > MaxSize {
		return nil, fmt.Errorf("env file %s: larger than %d bytes", path, MaxSize)
	}
	b := make([]byte, fi.Size())
	if _, err := f.ReadAt(b, 0); err != nil && fi.Size() > 0 {
		return nil, fmt.Errorf("env file %s: %w", path, err)
	}
	vars, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("env file %s: %w", path, err)
	}
	var set []string
	for k, v := range vars {
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return set, fmt.Errorf("env file %s: set %s: %w", path, k, err)
		}
		set = append(set, k)
	}
	return set, nil
}
