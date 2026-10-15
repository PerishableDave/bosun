// Package files is the first-party files integration: it gives the assistant
// access to only the folders the admin lists under `roots`, as the daemon's
// OS user. It is shared (one filesystem for the whole household), so every
// result is marked Untrusted: another household member, or anything syncing
// into those folders, may have written the text.
//
// Path safety: every path the model supplies has the form
// "<rootname>/<relative>". Absolute paths, "..", NUL, backslashes, over-long
// names and hidden components (".git", ".env", dotfiles) are rejected before
// touching the disk. Existing paths are resolved with filepath.EvalSymlinks and
// must stay inside the root's resolved path, so a symlink pointing outside is
// refused. Recursive list and search never follow symlinks. Errors never
// reveal an absolute host path.
package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/perishabledave/bosun/internal/integration"
)

// Settings is the "files" section of bosun.yaml.
type Settings struct {
	Roots []RootConfig `json:"roots"`
	// MaxRead caps what files.read returns and the size of files searched,
	// e.g. "2MiB" (default 2MiB). A bare number is bytes.
	MaxRead json.RawMessage `json:"max_read,omitempty"`
	// Extensions is an allowlist like [".md", ".txt"]; empty means any.
	Extensions []string `json:"extensions,omitempty"`
	// IncludeHidden allows dotfiles and dot-directories (.git, .env, ...).
	IncludeHidden bool `json:"include_hidden,omitempty"`
}

// RootConfig is one folder the assistant may touch.
type RootConfig struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Writable bool   `json:"writable"`
}

// Limits.
const (
	DefaultMaxRead   = 2 << 20
	maxMaxRead       = 64 << 20
	MaxWriteBytes    = 16 << 20
	maxNameBytes     = 255
	defaultListLimit = 500
	maxListLimit     = 1000
	defaultHits      = 50
	maxHits          = 200
	maxVisited       = 50_000
	maxLineLen       = 300
	sniffLen         = 8192
)

const (
	capRead   = integration.Capability("files.read")
	capWrite  = integration.Capability("files.write")
	capDelete = integration.Capability("files.delete")
)

var rootName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type root struct {
	name     string
	real     string // resolved, symlink-free absolute path
	writable bool
}

// Integration implements integration.Integration.
type Integration struct {
	roots         []*root
	byName        map[string]*root
	maxRead       int64
	exts          map[string]bool // nil = any
	extList       []string
	includeHidden bool
	inited        bool
}

// New returns an uninitialised files integration.
func New() *Integration { return &Integration{} }

var _ integration.Integration = (*Integration)(nil)

// Manifest implements integration.Integration.
func (f *Integration) Manifest() integration.Manifest {
	return integration.Manifest{
		Name:         "files",
		Version:      "0.1.0",
		Scope:        integration.ScopeShared,
		Capabilities: []integration.Capability{capRead, capWrite, capDelete},
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["roots"],"properties":{` +
			`"roots":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"required":["path","name"],"properties":{` +
			`"path":{"type":"string"},"name":{"type":"string"},"writable":{"type":"boolean","default":false}}}},` +
			`"max_read":{"type":["string","integer"],"description":"e.g. 2MiB"},` +
			`"extensions":{"type":"array","items":{"type":"string"}},` +
			`"include_hidden":{"type":"boolean","default":false}}}`),
	}
}

// ParseSize parses "2MiB", "512KiB", "1MB", "100K" or a bare byte count.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	var mult int64
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// Init implements integration.Integration.
func (f *Integration) Init(_ context.Context, cfg integration.Config, _ integration.CredentialSource) error {
	var s Settings
	if len(cfg.Settings) > 0 {
		dec := json.NewDecoder(bytes.NewReader(cfg.Settings))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("files: decode config: %w", err)
		}
	}
	if len(s.Roots) == 0 {
		return errors.New("files: at least one root is required")
	}
	f.maxRead = DefaultMaxRead
	if raw := bytes.TrimSpace(s.MaxRead); len(raw) > 0 && string(raw) != "null" {
		str := string(raw)
		if raw[0] == '"' {
			if err := json.Unmarshal(raw, &str); err != nil {
				return fmt.Errorf("files: max_read: %w", err)
			}
		}
		n, err := ParseSize(str)
		if err != nil {
			return fmt.Errorf("files: max_read: %w", err)
		}
		if n <= 0 || n > maxMaxRead {
			return fmt.Errorf("files: max_read must be between 1 byte and %d MiB", maxMaxRead>>20)
		}
		f.maxRead = n
	}
	f.exts, f.extList = nil, nil
	for _, e := range s.Extensions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		if f.exts == nil {
			f.exts = map[string]bool{}
		}
		if !f.exts[e] {
			f.exts[e] = true
			f.extList = append(f.extList, e)
		}
	}
	sort.Strings(f.extList)
	f.includeHidden = s.IncludeHidden
	f.roots, f.byName = nil, map[string]*root{}
	for i, rc := range s.Roots {
		if !rootName.MatchString(rc.Name) {
			return fmt.Errorf("files: roots[%d]: name %q must match %s", i, rc.Name, rootName)
		}
		if f.byName[rc.Name] != nil {
			return fmt.Errorf("files: duplicate root name %q", rc.Name)
		}
		p, err := expandHome(rc.Path)
		if err != nil {
			return fmt.Errorf("files: roots[%d]: %w", i, err)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("files: root %q: path %q must be absolute (or start with ~/)", rc.Name, rc.Path)
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fmt.Errorf("files: root %q: resolve %q: %w", rc.Name, rc.Path, err)
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			return fmt.Errorf("files: root %q: %q is not a directory", rc.Name, rc.Path)
		}
		r := &root{name: rc.Name, real: real, writable: rc.Writable}
		f.roots = append(f.roots, r)
		f.byName[r.name] = r
	}
	f.inited = true
	return nil
}

// Close implements integration.Integration.
func (f *Integration) Close() error { return nil }

// Tools implements integration.Integration.
func (f *Integration) Tools() []integration.Tool {
	obj := func(required string, props ...string) json.RawMessage {
		return json.RawMessage(`{"type":"object","additionalProperties":false,` + required + `"properties":{` + strings.Join(props, ",") + `}}`)
	}
	pathP := func(desc string) string {
		return `"path":{"type":"string","description":"` + desc + ` as <root>/<relative path>, e.g. \"shared/reports/q3.md\"; see files.roots"}`
	}
	req := func(names ...string) string {
		q := make([]string, len(names))
		for i, n := range names {
			q[i] = strconv.Quote(n)
		}
		return `"required":[` + strings.Join(q, ",") + `],`
	}
	return []integration.Tool{
		{
			Name:        "files.roots",
			Description: "List the folders the assistant may access (root name, whether it is writable, extension policy). Paths for the other files.* tools start with a root name.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: obj(""),
			Handler:     f.rootsTool,
		},
		{
			Name:        "files.list",
			Description: "List files and folders (name, size, modified time) under a root or folder. Hidden entries are skipped. Empty path lists the roots.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: obj("", pathP("folder"),
				`"recursive":{"type":"boolean","description":"descend into subfolders"}`,
				`"limit":{"type":"integer","minimum":1,"maximum":1000,"description":"maximum entries (default 500)"}`),
			Handler: f.list,
		},
		{
			Name:        "files.search",
			Description: "Find files by name glob and/or a case-insensitive text substring inside text files. Returns matching lines with context. Searches all roots unless root is given.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: obj("",
				`"root":{"type":"string","description":"limit to one root name"}`,
				`"name_glob":{"type":"string","description":"file name pattern, e.g. \"*.md\" (case-insensitive)"}`,
				`"content":{"type":"string","description":"text to find inside files"}`,
				`"limit":{"type":"integer","minimum":1,"maximum":200,"description":"maximum results (default 50)"}`),
			Handler: f.search,
		},
		{
			Name:        "files.read",
			Description: "Read a text file (UTF-8; at most the configured read cap per call, use offset to continue). Binary files, images and PDFs return only their type and size.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: obj(req("path"), pathP("file"),
				`"offset":{"type":"integer","minimum":0,"description":"byte offset to start at"}`,
				`"limit":{"type":"integer","minimum":1,"description":"maximum bytes to return"}`),
			Handler: f.read,
		},
		{
			Name:        "files.stat",
			Description: "Show type, size, modified time and detected content type of a file or folder.",
			Capability:  capRead, Risk: integration.RiskRead,
			InputSchema: obj(req("path"), pathP("file or folder")),
			Handler:     f.stat,
		},
		{
			Name:        "files.write",
			Description: "Create a text file, or replace an existing file's entire content when overwrite=true (otherwise an existing file is refused). Atomic. Only in writable roots.",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: obj(req("path", "content"), pathP("file"),
				`"content":{"type":"string"}`,
				`"overwrite":{"type":"boolean","description":"replace the file if it already exists"}`,
				`"create_dirs":{"type":"boolean","description":"create missing parent folders"}`),
			Handler: f.write,
		},
		{
			Name:        "files.append",
			Description: "Append text to the end of a file (created if missing, parent folder must exist). Only in writable roots.",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: obj(req("path", "content"), pathP("file"), `"content":{"type":"string","minLength":1}`),
			Handler:     f.append,
		},
		{
			Name:        "files.move",
			Description: "Rename or move a file or folder within the same root. The destination must not exist. Only in writable roots.",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: obj(req("from", "to"),
				`"from":{"type":"string","description":"existing path as <root>/<relative>"}`,
				`"to":{"type":"string","description":"new path in the same root"}`),
			Handler: f.move,
		},
		{
			Name:        "files.mkdir",
			Description: "Create a folder (and missing parents). Only in writable roots.",
			Capability:  capWrite, Risk: integration.RiskWrite,
			InputSchema: obj(req("path"), pathP("folder")),
			Handler:     f.mkdir,
		},
		{
			Name:        "files.delete",
			Description: "Delete one file permanently. Folders cannot be deleted. Only in writable roots.",
			Capability:  capDelete, Risk: integration.RiskDestructive,
			InputSchema: obj(req("path"), pathP("file")),
			Handler:     f.delete,
		},
	}
}

// --- path safety ---

// errPath is wrapped by every rejected path.
var errPath = errors.New("invalid path")

func pathErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errPath, fmt.Sprintf(format, a...))
}

func (f *Integration) rootNames() string {
	n := make([]string, len(f.roots))
	for i, r := range f.roots {
		n[i] = r.name
	}
	return strings.Join(n, ", ")
}

// parsePath validates a model-supplied "<root>/<rel>" path purely
// lexically. rel is "." for the root itself and uses OS separators.
func (f *Integration) parsePath(in string) (*root, string, error) {
	if !f.inited {
		return nil, "", errors.New("files: not initialised")
	}
	p := strings.TrimSpace(in)
	if p == "" {
		return nil, "", pathErr("path is required (start with a root name: %s)", f.rootNames())
	}
	if strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return nil, "", pathErr("path contains forbidden characters")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || filepath.VolumeName(p) != "" {
		return nil, "", pathErr("path must be <root>/<relative path>, not an absolute path (roots: %s)", f.rootNames())
	}
	var parts []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c == ".." {
			return nil, "", pathErr("path must not contain ..")
		}
		if len(c) > maxNameBytes {
			return nil, "", pathErr("a path component is longer than %d bytes", maxNameBytes)
		}
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return nil, "", pathErr("path is required (start with a root name: %s)", f.rootNames())
	}
	r := f.byName[parts[0]]
	if r == nil {
		return nil, "", pathErr("unknown root %q (roots: %s)", parts[0], f.rootNames())
	}
	rest := parts[1:]
	if !f.includeHidden {
		for _, c := range rest {
			if strings.HasPrefix(c, ".") {
				return nil, "", pathErr("%q is hidden and not accessible", c)
			}
		}
	}
	if len(rest) == 0 {
		return r, ".", nil
	}
	return r, filepath.Join(rest...), nil
}

func display(r *root, rel string) string {
	if rel == "." {
		return r.name + "/"
	}
	return r.name + "/" + filepath.ToSlash(rel)
}

func within(root, p string) bool {
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (f *Integration) extOK(name string) bool {
	return f.exts == nil || f.exts[strings.ToLower(filepath.Ext(name))]
}

func (f *Integration) checkExt(rel string) error {
	if f.extOK(rel) {
		return nil
	}
	return pathErr("%q: only %s files are allowed", filepath.ToSlash(rel), strings.Join(f.extList, ", "))
}

func (f *Integration) hiddenIn(r *root, real string) bool {
	if f.includeHidden {
		return false
	}
	rel, err := filepath.Rel(r.real, real)
	if err != nil || rel == "." {
		return false
	}
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(c, ".") {
			return true
		}
	}
	return false
}

// existing resolves rel (which must exist) following symlinks, and rejects
// anything outside the root.
func (f *Integration) existing(r *root, rel, shown string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(r.real, rel))
	if err != nil {
		return "", fmt.Errorf("%s: %s", shown, osErr(err))
	}
	if !within(r.real, real) {
		return "", pathErr("%q resolves outside its root", shown)
	}
	if f.hiddenIn(r, real) {
		return "", pathErr("%q resolves to a hidden path", shown)
	}
	return real, nil
}

// resolveFile resolves an existing regular file, applying the extension
// policy to both the name and any symlink target.
func (f *Integration) resolveFile(in string) (*root, string, string, os.FileInfo, error) {
	r, rel, err := f.parsePath(in)
	if err != nil {
		return nil, "", "", nil, err
	}
	shown := display(r, rel)
	if rel == "." {
		return nil, "", "", nil, pathErr("%q is a folder", shown)
	}
	if err := f.checkExt(rel); err != nil {
		return nil, "", "", nil, err
	}
	real, err := f.existing(r, rel, shown)
	if err != nil {
		return nil, "", "", nil, err
	}
	if !f.extOK(real) {
		return nil, "", "", nil, pathErr("%q links to a file type that is not allowed", shown)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("%s: %s", shown, osErr(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, "", "", nil, pathErr("%q is not a regular file", shown)
	}
	return r, rel, real, fi, nil
}

func (r *root) mustWrite() error {
	if !r.writable {
		return fmt.Errorf("root %q is read-only", r.name)
	}
	return nil
}

// parentOf validates the parent folder of rel (non-root) for a create or
// rename. With createDirs, missing parents are created, but only after the
// deepest existing ancestor is proven to be inside the root. It returns the
// resolved parent directory and the final name.
func (f *Integration) parentOf(r *root, rel, shown string, createDirs bool) (string, string, error) {
	if rel == "." {
		return "", "", pathErr("%q is a root; name a file or folder inside it", shown)
	}
	full := filepath.Join(r.real, rel)
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
	if err != nil || !within(r.real, realAnc) {
		return "", "", pathErr("%q resolves outside its root", shown)
	}
	if anc != parent {
		if !createDirs {
			return "", "", fmt.Errorf("%s: parent folder does not exist (create_dirs=true creates it)", shown)
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", "", fmt.Errorf("%s: create folder: %s", shown, osErr(err))
		}
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil || !within(r.real, realParent) {
		return "", "", pathErr("%q resolves outside its root", shown)
	}
	if fi, err := os.Stat(realParent); err != nil || !fi.IsDir() {
		return "", "", pathErr("parent of %q is not a folder", shown)
	}
	return realParent, filepath.Base(full), nil
}

// osErr turns an OS error into a message without any host path.
func osErr(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrExist):
		return "already exists"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	return err.Error()
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

func result(content string, data any) integration.Result {
	return integration.Result{Content: content, Untrusted: true, Data: data}
}

func human(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

// looksBinary reports whether b (a file prefix) is not text.
func looksBinary(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	bad := 0
	for _, r := range string(bytes.ToValidUTF8(b, []byte("�"))) {
		if r == unicode.ReplacementChar || (r < 0x20 && r != '\n' && r != '\r' && r != '\t' && r != '\f') {
			bad++
		}
	}
	return len(b) > 0 && bad*3 > len(b)
}
