// Package notes is the first-party notes integration: Markdown and text
// files in a directory, either shared by the household or split per user
// (<dir>/u<id>/, keyed by numeric user ID so a deleted user's
// notes are never inherited by a later user with the same name).
//
// Path safety: every path is relative to the notes root; absolute paths, ".."
// escapes, hidden components (".git", ".obsidian"), and symlinks resolving
// outside the root are rejected, and only .md, .markdown and .txt files are
// readable or writable.
//
// Trust: in per-user mode the notes are the calling user's own data, so
// results are trusted. In shared mode any household member (or anything
// syncing into the directory) may have written the text, so every result is
// marked Untrusted.
package notes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/integration"
)

// Settings is the "notes" section of bosun.yaml.
type Settings struct {
	// Dir is the notes root. Empty means the integration's private data
	// directory (<data_dir>/integrations/notes).
	Dir string `json:"dir,omitempty"`
	// PerUser gives each user their own subdirectory <dir>/u<id>/
	// (default true). false shares one directory with every user.
	PerUser *bool `json:"per_user,omitempty"`
}

// Limits.
const (
	MaxReadBytes     = 200_000 // notes.read returns at most this much
	MaxWriteBytes    = 1 << 20 // notes.write / notes.append content limit
	maxListEntries   = 1000
	maxSearchFile    = 2 << 20 // files larger than this are skipped by search
	defaultMaxHits   = 50
	maxMaxHits       = 200
	maxContextLines  = 5
	maxSearchLineLen = 500
)

const (
	capRead   = integration.Capability("notes.read")
	capWrite  = integration.Capability("notes.write")
	capDelete = integration.Capability("notes.delete")
)

var allowedExt = map[string]bool{".md": true, ".markdown": true, ".txt": true}

// UserDirFunc returns the legacy directory name (the username) for a user's
// notes. Per-user folders are now <dir>/u<id>/; the name is used only to
// migrate an existing <dir>/<name>/ folder the first time the user's notes
// are touched.
type UserDirFunc func(ctx context.Context, userID int64) (string, error)

// Integration implements integration.Integration.
type Integration struct {
	root    string // resolved (symlink-free) absolute root
	perUser bool
	userDir UserDirFunc
	inited  bool
	migMu   sync.Mutex
}

// New returns an uninitialised notes integration. In per-user mode users'
// directories are named u<id> after their numeric user ID.
func New() *Integration { return &Integration{perUser: true} }

var _ integration.Integration = (*Integration)(nil)

// SetUserDirFunc sets the legacy-name lookup used to migrate pre-existing
// <dir>/<username>/ folders to <dir>/u<id>/ (call before Init).
func (n *Integration) SetUserDirFunc(f UserDirFunc) { n.userDir = f }

// Manifest implements integration.Integration. The scope follows the
// per_user setting, so it is only final after Init (before Init it reports
// the default, per-user). Notes use no credentials.
func (n *Integration) Manifest() integration.Manifest {
	scope := integration.ScopeShared
	if n.perUser {
		scope = integration.ScopePerUser
	}
	return integration.Manifest{
		Name:         "notes",
		Version:      "0.1.0",
		Scope:        scope,
		Capabilities: []integration.Capability{capRead, capWrite, capDelete},
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
			`"dir":{"type":"string","description":"notes directory"},` +
			`"per_user":{"type":"boolean","default":true,"description":"each user's notes live in <dir>/u<id>/"}}}`),
	}
}

// Init implements integration.Integration.
func (n *Integration) Init(_ context.Context, cfg integration.Config, _ integration.CredentialSource) error {
	var s Settings
	if len(cfg.Settings) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("notes: decode config: %w", err)
		}
	}
	dir := s.Dir
	if dir == "" {
		dir = cfg.DataDir
	}
	if dir == "" {
		return errors.New("notes: dir is required")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("notes: dir %q must be an absolute path", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("notes: create dir: %w", err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("notes: resolve dir: %w", err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("notes: %s is not a directory", dir)
	}
	n.root = root
	n.perUser = s.PerUser == nil || *s.PerUser
	n.inited = true
	return nil
}

// Close implements integration.Integration.
func (n *Integration) Close() error { return nil }

// Tools implements integration.Integration.
func (n *Integration) Tools() []integration.Tool {
	pathProp := `"path":{"type":"string","minLength":1,"description":"note path relative to the notes folder, e.g. \"journal/2026-10-13.md\" (.md, .markdown or .txt)"}`
	return []integration.Tool{
		{
			Name:        "notes.list",
			Description: "List the user's notes (relative paths, size, modified time), optionally under a subfolder.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` +
				`"dir":{"type":"string","description":"subfolder to list (default: all notes)"}}}`),
			Handler: n.list,
		},
		{
			Name:        "notes.search",
			Description: "Search the contents of the user's notes (case-insensitive substring, or a regular expression with regex=true). Returns matching lines with surrounding context.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{` +
				`"query":{"type":"string","minLength":1},` +
				`"regex":{"type":"boolean","description":"treat query as a regular expression (RE2 syntax)"},` +
				`"dir":{"type":"string","description":"limit the search to this subfolder"},` +
				`"context":{"type":"integer","minimum":0,"maximum":5,"description":"lines of context around each match (default 1)"},` +
				`"max_results":{"type":"integer","minimum":1,"maximum":200,"description":"maximum matches (default 50)"}}}`),
			Handler: n.search,
		},
		{
			Name:        "notes.read",
			Description: "Read one note.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{` + pathProp + `}}`),
			Handler:     n.read,
		},
		{
			Name:        "notes.write",
			Description: "Create a note or replace an existing note's entire content. Parent folders are created as needed.",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","content"],"properties":{` +
				pathProp + `,"content":{"type":"string"}}}`),
			Handler: n.write,
		},
		{
			Name:        "notes.append",
			Description: "Append text to the end of a note (created if missing), starting on a new line.",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","content"],"properties":{` +
				pathProp + `,"content":{"type":"string","minLength":1}}}`),
			Handler: n.append,
		},
		{
			Name:        "notes.delete",
			Description: "Delete one note permanently.",
			Capability:  capDelete, Risk: integration.RiskDestructive,
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{` + pathProp + `}}`),
			Handler:     n.delete,
		},
	}
}

// --- roots and path safety ---

// errPath is returned (wrapped) for every rejected path.
var errPath = errors.New("invalid note path")

var safeDirName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// userRoot returns the resolved root the calling user may access, creating
// it in per-user mode.
func (n *Integration) userRoot(ctx context.Context) (string, error) {
	if !n.inited {
		return "", errors.New("notes: not initialised")
	}
	uid, ok := integration.UserIDFromContext(ctx)
	if !ok || uid <= 0 {
		return "", errors.New("notes: no user in context")
	}
	if !n.perUser {
		return n.root, nil
	}
	name := "u" + strconv.FormatInt(uid, 10)
	dir := filepath.Join(n.root, name)
	n.migrateLegacy(ctx, uid, dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("notes: create user directory: %w", err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("notes: resolve user directory: %w", err)
	}
	if !within(n.root, real) || real == n.root {
		return "", fmt.Errorf("notes: user directory %q escapes the notes root", name)
	}
	return real, nil
}

var idDirName = regexp.MustCompile(`^u[0-9]+$`)

// migrateLegacy renames an old <root>/<username>/ folder to dir (<root>/u<id>)
// the first time the user's notes are touched. It never overwrites an
// existing dir and is best-effort: failures are logged and the user simply
// starts with an empty folder.
func (n *Integration) migrateLegacy(ctx context.Context, uid int64, dir string) {
	if n.userDir == nil {
		return
	}
	n.migMu.Lock()
	defer n.migMu.Unlock()
	if _, err := os.Lstat(dir); err == nil {
		return // already migrated or created
	}
	old, err := n.userDir(ctx, uid)
	if err != nil || !safeDirName.MatchString(old) || strings.Contains(old, "..") || idDirName.MatchString(old) {
		return
	}
	oldPath := filepath.Join(n.root, old)
	fi, err := os.Lstat(oldPath)
	if err != nil || !fi.IsDir() { // missing, or a symlink/file: leave alone
		return
	}
	if err := os.Rename(oldPath, dir); err != nil {
		slog.Warn("notes: migrating per-user folder failed", "from", oldPath, "to", dir, "err", err)
		return
	}
	slog.Info("notes: migrated per-user folder to its user ID", "from", oldPath, "to", dir, "user_id", uid)
}

// within reports whether p is root or inside it (both cleaned, absolute).
func within(root, p string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// cleanRel validates a model-supplied relative path and returns it cleaned
// (OS separators). An empty rel is allowed only if allowEmpty (meaning the
// root itself).
func cleanRel(rel string, allowEmpty bool) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		if allowEmpty {
			return ".", nil
		}
		return "", fmt.Errorf("%w: path is required", errPath)
	}
	if strings.ContainsRune(rel, 0) || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("%w: %q contains forbidden characters", errPath, rel)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("%w: %q must be relative to the notes folder", errPath, rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q must not contain ..", errPath, rel)
		}
		if strings.HasPrefix(part, ".") && part != "." {
			return "", fmt.Errorf("%w: %q refers to a hidden file or folder", errPath, rel)
		}
	}
	c := filepath.Clean(filepath.FromSlash(rel))
	if c == "." && !allowEmpty {
		return "", fmt.Errorf("%w: path is required", errPath)
	}
	return c, nil
}

func checkExt(p string) error {
	if !allowedExt[strings.ToLower(filepath.Ext(p))] {
		return fmt.Errorf("%w: %q: only .md, .markdown and .txt files are allowed", errPath, filepath.ToSlash(p))
	}
	return nil
}

// resolveExisting resolves rel (which must exist) under root, following
// symlinks, and rejects anything that ends up outside root.
func resolveExisting(root, rel string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("note %q not found", filepath.ToSlash(rel))
	}
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	if !within(root, real) {
		return "", fmt.Errorf("%w: %q resolves outside the notes folder", errPath, filepath.ToSlash(rel))
	}
	return real, nil
}

// noteFile resolves an existing note file.
func noteFile(root, relIn string) (string, string, error) {
	rel, err := cleanRel(relIn, false)
	if err != nil {
		return "", "", err
	}
	if err := checkExt(rel); err != nil {
		return "", "", err
	}
	real, err := resolveExisting(root, rel)
	if err != nil {
		return "", "", err
	}
	if !allowedExt[strings.ToLower(filepath.Ext(real))] { // a symlink must point at a note too
		return "", "", fmt.Errorf("%w: %q links to a file that is not a note", errPath, filepath.ToSlash(rel))
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", "", fmt.Errorf("stat %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	if !fi.Mode().IsRegular() {
		return "", "", fmt.Errorf("%w: %q is not a regular file", errPath, filepath.ToSlash(rel))
	}
	return real, rel, nil
}

// noteTarget resolves a note path for writing: the file may not exist yet.
// Missing parent folders are created (inside root only).
func noteTarget(root, relIn string) (string, string, error) {
	rel, err := cleanRel(relIn, false)
	if err != nil {
		return "", "", err
	}
	if err := checkExt(rel); err != nil {
		return "", "", err
	}
	full := filepath.Join(root, rel)
	if _, err := os.Lstat(full); err == nil {
		// Exists (possibly as a symlink): it must resolve to a regular note
		// inside root.
		real, _, err := noteFile(root, relIn)
		return real, rel, err
	}
	// Find the deepest existing ancestor and make sure it is inside root
	// before creating anything.
	parent := filepath.Dir(full)
	anc := parent
	for {
		if _, err := os.Lstat(anc); err == nil {
			break
		}
		next := filepath.Dir(anc)
		if next == anc {
			break
		}
		anc = next
	}
	realAnc, err := filepath.EvalSymlinks(anc)
	if err != nil || !within(root, realAnc) {
		return "", "", fmt.Errorf("%w: %q resolves outside the notes folder", errPath, filepath.ToSlash(rel))
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", "", fmt.Errorf("create folder for %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil || !within(root, realParent) {
		return "", "", fmt.Errorf("%w: %q resolves outside the notes folder", errPath, filepath.ToSlash(rel))
	}
	if fi, err := os.Stat(realParent); err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("%w: parent of %q is not a folder", errPath, filepath.ToSlash(rel))
	}
	return filepath.Join(realParent, filepath.Base(full)), rel, nil
}

// noteDir resolves an optional subfolder for list/search.
func noteDir(root, relIn string) (string, string, error) {
	rel, err := cleanRel(relIn, true)
	if err != nil {
		return "", "", err
	}
	real, err := resolveExisting(root, rel)
	if err != nil {
		return "", "", err
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("%q is not a folder", filepath.ToSlash(rel))
	}
	return real, rel, nil
}

// walkNotes calls fn for every regular note file under dir (skipping hidden
// entries and symlinks), with its path relative to root.
func walkNotes(root, dir string, fn func(rel string, fi fs.FileInfo) error) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable entry: skip
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() || !allowedExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		return fn(filepath.ToSlash(rel), fi)
	})
}

// --- tools ---

// Entry is one note in notes.list (Result.Data is []Entry).
type Entry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

var errStop = errors.New("stop")

func (n *Integration) result(content string, data any) integration.Result {
	return integration.Result{Content: content, Untrusted: !n.perUser, Data: data}
}

func decodeArgs(in json.RawMessage, v any) error {
	if len(in) == 0 {
		return nil
	}
	if err := json.Unmarshal(in, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (n *Integration) list(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Dir string `json:"dir"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	root, err := n.userRoot(ctx)
	if err != nil {
		return integration.Result{}, err
	}
	dir, _, err := noteDir(root, a.Dir)
	if err != nil {
		return integration.Result{}, err
	}
	var entries []Entry
	truncated := false
	err = walkNotes(root, dir, func(rel string, fi fs.FileInfo) error {
		if len(entries) == maxListEntries {
			truncated = true
			return errStop
		}
		entries = append(entries, Entry{Path: rel, Size: fi.Size(), Modified: fi.ModTime().UTC().Truncate(time.Second)})
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return integration.Result{}, fmt.Errorf("list notes: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if len(entries) == 0 {
		return n.result("No notes found.", []Entry{}), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d note(s):\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%d bytes\tmodified %s\n", e.Path, e.Size, e.Modified.Format(time.RFC3339))
	}
	if truncated {
		fmt.Fprintf(&b, "(listing truncated at %d notes; list a subfolder to see more)\n", maxListEntries)
	}
	return n.result(b.String(), entries), nil
}

// Match is one notes.search hit (Result.Data is []Match).
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"` // 1-based line of the match
	// Context holds the matching line and its surrounding lines, in order,
	// starting at line StartLine.
	StartLine int      `json:"start_line"`
	Context   []string `json:"context"`
}

func (n *Integration) search(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Query      string `json:"query"`
		Regex      bool   `json:"regex"`
		Dir        string `json:"dir"`
		Context    *int   `json:"context"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if a.Query == "" {
		return integration.Result{}, errors.New("query is required")
	}
	ctxLines := 1
	if a.Context != nil {
		ctxLines = min(max(*a.Context, 0), maxContextLines)
	}
	maxHits := defaultMaxHits
	if a.MaxResults > 0 {
		maxHits = min(a.MaxResults, maxMaxHits)
	}
	var match func(string) bool
	if a.Regex {
		re, err := regexp.Compile("(?i)" + a.Query)
		if err != nil {
			return integration.Result{}, fmt.Errorf("invalid regular expression: %w", err)
		}
		match = re.MatchString
	} else {
		q := strings.ToLower(a.Query)
		match = func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	}
	root, err := n.userRoot(ctx)
	if err != nil {
		return integration.Result{}, err
	}
	dir, _, err := noteDir(root, a.Dir)
	if err != nil {
		return integration.Result{}, err
	}
	var hits []Match
	truncated := false
	err = walkNotes(root, dir, func(rel string, fi fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fi.Size() > maxSearchFile {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil
		}
		lines := splitLines(b)
		for i, l := range lines {
			if !match(l) {
				continue
			}
			if len(hits) == maxHits {
				truncated = true
				return errStop
			}
			lo, hi := max(i-ctxLines, 0), min(i+ctxLines+1, len(lines))
			c := make([]string, 0, hi-lo)
			for _, s := range lines[lo:hi] {
				c = append(c, truncLine(s))
			}
			hits = append(hits, Match{Path: rel, Line: i + 1, StartLine: lo + 1, Context: c})
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return integration.Result{}, fmt.Errorf("search notes: %w", err)
	}
	if len(hits) == 0 {
		return n.result(fmt.Sprintf("No notes match %q.", a.Query), []Match{}), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q:\n", len(hits), a.Query)
	for _, h := range hits {
		fmt.Fprintf(&b, "\n%s:%d\n", h.Path, h.Line)
		for k, s := range h.Context {
			mark := " "
			if h.StartLine+k == h.Line {
				mark = ">"
			}
			fmt.Fprintf(&b, "%s %d: %s\n", mark, h.StartLine+k, s)
		}
	}
	if truncated {
		fmt.Fprintf(&b, "\n(stopped after %d matches; narrow the query or raise max_results)\n", maxHits)
	}
	return n.result(b.String(), hits), nil
}

func splitLines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), maxSearchFile)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

func truncLine(s string) string {
	if len(s) <= maxSearchLineLen {
		return s
	}
	cut := maxSearchLineLen
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func (n *Integration) read(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	root, err := n.userRoot(ctx)
	if err != nil {
		return integration.Result{}, err
	}
	real, rel, err := noteFile(root, a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	f, err := os.Open(real)
	if err != nil {
		return integration.Result{}, fmt.Errorf("open %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, MaxReadBytes+1))
	if err != nil {
		return integration.Result{}, fmt.Errorf("read %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	k := len(buf)
	content := string(buf[:min(k, MaxReadBytes)])
	if k > MaxReadBytes {
		for len(content) > 0 && !utf8Start(content[len(content)-1]) {
			content = content[:len(content)-1]
		}
		content += fmt.Sprintf("\n\n[truncated: note exceeds %d bytes]", MaxReadBytes)
	}
	return n.result(content, map[string]any{"path": filepath.ToSlash(rel), "truncated": k > MaxReadBytes}), nil
}

// osErr strips the absolute path from *fs.PathError so error text shown to
// the model only carries the relative note path.
func osErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (n *Integration) write(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if a.Content == nil {
		return integration.Result{}, errors.New("content is required")
	}
	if len(*a.Content) > MaxWriteBytes {
		return integration.Result{}, fmt.Errorf("content exceeds %d bytes", MaxWriteBytes)
	}
	root, err := n.userRoot(ctx)
	if err != nil {
		return integration.Result{}, err
	}
	target, rel, err := noteTarget(root, a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	_, statErr := os.Stat(target)
	if err := os.WriteFile(target, []byte(*a.Content), 0o600); err != nil {
		return integration.Result{}, fmt.Errorf("write %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	verb := "Updated"
	if statErr != nil {
		verb = "Created"
	}
	return n.result(fmt.Sprintf("%s %s (%d bytes).", verb, filepath.ToSlash(rel), len(*a.Content)),
		map[string]any{"path": filepath.ToSlash(rel), "created": statErr != nil}), nil
}

func (n *Integration) append(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if a.Content == "" {
		return integration.Result{}, errors.New("content is required")
	}
	if len(a.Content) > MaxWriteBytes {
		return integration.Result{}, fmt.Errorf("content exceeds %d bytes", MaxWriteBytes)
	}
	root, err := n.userRoot(ctx)
	if err != nil {
		return integration.Result{}, err
	}
	target, rel, err := noteTarget(root, a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return integration.Result{}, fmt.Errorf("open %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return integration.Result{}, fmt.Errorf("stat %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	text := a.Content
	if size := fi.Size(); size > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, size-1); err == nil && last[0] != '\n' {
			text = "\n" + text
		}
	}
	if _, err := f.WriteString(text); err != nil {
		return integration.Result{}, fmt.Errorf("append to %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	return n.result(fmt.Sprintf("Appended %d bytes to %s.", len(a.Content), filepath.ToSlash(rel)),
		map[string]any{"path": filepath.ToSlash(rel)}), nil
}

func (n *Integration) delete(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	root, err := n.userRoot(ctx)
	if err != nil {
		return integration.Result{}, err
	}
	// Validate the link target too, but delete the named entry itself (a
	// symlink is removed, not what it points to).
	if _, _, err := noteFile(root, a.Path); err != nil {
		return integration.Result{}, err
	}
	rel, _ := cleanRel(a.Path, false)
	if err := os.Remove(filepath.Join(root, rel)); err != nil {
		return integration.Result{}, fmt.Errorf("delete %q: %w", filepath.ToSlash(rel), osErr(err))
	}
	return n.result(fmt.Sprintf("Deleted %s.", filepath.ToSlash(rel)), map[string]any{"path": filepath.ToSlash(rel)}), nil
}
