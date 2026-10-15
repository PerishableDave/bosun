package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // register decoders for files.read image dimensions
	_ "image/jpeg" // ditto
	_ "image/png"  // ditto
	"io"
	"io/fs"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/perishabledave/bosun/internal/integration"
)

var errStop = errors.New("stop")

// walk calls fn for every non-hidden, non-symlink entry under start (a
// resolved directory inside r), with its path relative to r.real. Symlinks
// are skipped entirely so a walk can never leave the root. With recursive
// false only start's direct children are visited. It stops with errStop
// semantics when fn returns errStop, and after maxVisited entries.
func (f *Integration) walk(r *root, start string, recursive bool, fn func(rel string, d fs.DirEntry) error) (truncated bool, err error) {
	visited := 0
	err = filepath.WalkDir(start, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if p == start {
				return werr
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil // unreadable entry: skip
		}
		if p == start {
			return nil
		}
		if !f.includeHidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if visited++; visited > maxVisited {
			truncated = true
			return errStop
		}
		rel, rerr := filepath.Rel(r.real, p)
		if rerr != nil {
			return nil
		}
		if ferr := fn(rel, d); ferr != nil {
			return ferr
		}
		if d.IsDir() && !recursive {
			return fs.SkipDir
		}
		return nil
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	return truncated, err
}

// Entry is one item in files.list (Result.Data is []Entry).
type Entry struct {
	Path     string    `json:"path"`
	Dir      bool      `json:"dir,omitempty"`
	Size     int64     `json:"size,omitempty"`
	Modified time.Time `json:"modified"`
}

func (f *Integration) rootsTool(_ context.Context, _ json.RawMessage) (integration.Result, error) {
	if !f.inited {
		return integration.Result{}, errors.New("files: not initialised")
	}
	type info struct {
		Name       string   `json:"name"`
		Writable   bool     `json:"writable"`
		Extensions []string `json:"extensions"`
		MaxRead    int64    `json:"max_read"`
	}
	exts := f.extList
	if exts == nil {
		exts = []string{}
	}
	var out []info
	var b strings.Builder
	policy := "any file type"
	if len(exts) > 0 {
		policy = "only " + strings.Join(exts, " ")
	}
	fmt.Fprintf(&b, "%d root(s); extension policy: %s; read cap %s per call; hidden files %s.\n",
		len(f.roots), policy, human(f.maxRead), map[bool]string{true: "included", false: "not accessible"}[f.includeHidden])
	for _, r := range f.roots {
		mode := "read-only"
		if r.writable {
			mode = "writable"
		}
		fmt.Fprintf(&b, "- %s/ (%s)\n", r.name, mode)
		out = append(out, info{Name: r.name, Writable: r.writable, Extensions: exts, MaxRead: f.maxRead})
	}
	return result(strings.TrimRight(b.String(), "\n"), out), nil
}

func (f *Integration) list(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if strings.TrimSpace(a.Path) == "" {
		return f.rootsTool(ctx, nil)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	r, rel, err := f.parsePath(a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	shown := display(r, rel)
	real, err := f.existing(r, rel, shown)
	if err != nil {
		return integration.Result{}, err
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return integration.Result{}, pathErr("%q is not a folder", shown)
	}
	var entries []Entry
	truncated := false
	vt, err := f.walk(r, real, a.Recursive, func(p string, d fs.DirEntry) error {
		if !d.IsDir() && (!d.Type().IsRegular() || !f.extOK(p)) {
			return nil
		}
		if len(entries) >= limit {
			truncated = true
			return errStop
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		e := Entry{Path: r.name + "/" + filepath.ToSlash(p), Dir: d.IsDir(), Modified: fi.ModTime().UTC().Truncate(time.Second)}
		if !d.IsDir() {
			e.Size = fi.Size()
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return integration.Result{}, fmt.Errorf("list %s: %s", shown, osErr(err))
	}
	truncated = truncated || vt
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if len(entries) == 0 {
		return result("No files found in "+shown, []Entry{}), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d entr(ies) in %s:\n", len(entries), shown)
	for _, e := range entries {
		if e.Dir {
			fmt.Fprintf(&b, "%s/  (folder, modified %s)\n", e.Path, e.Modified.Format("2006-01-02 15:04"))
		} else {
			fmt.Fprintf(&b, "%s  (%s, modified %s)\n", e.Path, human(e.Size), e.Modified.Format("2006-01-02 15:04"))
		}
	}
	if truncated {
		b.WriteString("[list truncated; narrow the path or raise limit]\n")
	}
	return result(strings.TrimRight(b.String(), "\n"), entries), nil
}

// Match is one files.search hit (Result.Data is []Match).
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

func clip(s string) string {
	s = strings.TrimRight(s, "\r")
	if len(s) > maxLineLen {
		s = strings.ToValidUTF8(s[:maxLineLen], "") + "..."
	}
	return s
}

func (f *Integration) search(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Root     string `json:"root"`
		NameGlob string `json:"name_glob"`
		Content  string `json:"content"`
		Limit    int    `json:"limit"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if !f.inited {
		return integration.Result{}, errors.New("files: not initialised")
	}
	if a.NameGlob == "" && a.Content == "" {
		return integration.Result{}, errors.New("give name_glob and/or content")
	}
	glob := strings.ToLower(a.NameGlob)
	if glob != "" {
		if _, err := pathpkg.Match(glob, ""); err != nil {
			return integration.Result{}, fmt.Errorf("invalid name_glob: %w", err)
		}
	}
	needle := strings.ToLower(a.Content)
	limit := a.Limit
	if limit <= 0 {
		limit = defaultHits
	}
	if limit > maxHits {
		limit = maxHits
	}
	roots := f.roots
	if a.Root != "" {
		r := f.byName[strings.Trim(a.Root, "/ ")]
		if r == nil {
			return integration.Result{}, pathErr("unknown root %q (roots: %s)", a.Root, f.rootNames())
		}
		roots = []*root{r}
	}
	var matches []Match
	var b strings.Builder
	truncated := false
	for _, r := range roots {
		vt, err := f.walk(r, r.real, true, func(rel string, d fs.DirEntry) error {
			if len(matches) >= limit {
				truncated = true
				return errStop
			}
			slash := filepath.ToSlash(rel)
			shown := r.name + "/" + slash
			if glob != "" {
				target := strings.ToLower(d.Name())
				if strings.Contains(glob, "/") {
					target = strings.ToLower(slash)
				}
				if ok, _ := pathpkg.Match(glob, target); !ok {
					return nil
				}
			}
			if d.IsDir() {
				if needle == "" {
					matches = append(matches, Match{Path: shown + "/"})
					fmt.Fprintf(&b, "%s/ (folder)\n", shown)
				}
				return nil
			}
			if !d.Type().IsRegular() || !f.extOK(rel) {
				return nil
			}
			if needle == "" {
				matches = append(matches, Match{Path: shown})
				fmt.Fprintf(&b, "%s\n", shown)
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() > f.maxRead {
				return nil
			}
			data, err := os.ReadFile(filepath.Join(r.real, rel))
			if err != nil || looksBinary(data[:min(len(data), sniffLen)]) {
				return nil
			}
			lines := strings.Split(string(bytes.ToValidUTF8(data, []byte("\uFFFD"))), "\n")
			for i, ln := range lines {
				if !strings.Contains(strings.ToLower(ln), needle) {
					continue
				}
				if len(matches) >= limit {
					truncated = true
					return errStop
				}
				matches = append(matches, Match{Path: shown, Line: i + 1, Text: clip(ln)})
				fmt.Fprintf(&b, "%s:%d: %s\n", shown, i+1, clip(ln))
				if i > 0 {
					fmt.Fprintf(&b, "    %d- %s\n", i, clip(lines[i-1]))
				}
				if i+1 < len(lines) {
					fmt.Fprintf(&b, "    %d- %s\n", i+2, clip(lines[i+1]))
				}
			}
			return nil
		})
		if vt {
			truncated = true
		}
		if err != nil {
			return integration.Result{}, fmt.Errorf("search %s: %s", r.name, osErr(err))
		}
		if len(matches) >= limit && truncated {
			break
		}
	}
	if len(matches) == 0 {
		return result("No matches.", []Match{}), nil
	}
	out := fmt.Sprintf("%d match(es):\n%s", len(matches), strings.TrimRight(b.String(), "\n"))
	if truncated {
		out += "\n[results truncated; narrow the search or raise limit]"
	}
	return result(out, matches), nil
}

func (f *Integration) read(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if a.Offset < 0 || a.Limit < 0 {
		return integration.Result{}, errors.New("offset and limit must not be negative")
	}
	r, rel, real, fi, err := f.resolveFile(a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	shown := display(r, rel)
	fh, err := os.Open(real)
	if err != nil {
		return integration.Result{}, fmt.Errorf("%s: %s", shown, osErr(err))
	}
	defer fh.Close()
	prefix := make([]byte, sniffLen)
	pn, _ := io.ReadFull(fh, prefix)
	prefix = prefix[:pn]
	size := fi.Size()
	ext := strings.ToLower(filepath.Ext(real))
	meta := map[string]any{"path": shown, "size": size}

	if ext == ".pdf" || bytes.HasPrefix(prefix, []byte("%PDF-")) {
		meta["type"] = "application/pdf"
		return result(fmt.Sprintf("%s is a PDF (%s). Bosun cannot extract PDF text itself; the sandbox integration, if enabled, can process it.", shown, human(size)), meta), nil
	}
	if strings.HasPrefix(http.DetectContentType(prefix), "image/") {
		ct := http.DetectContentType(prefix)
		meta["type"] = ct
		if _, err := fh.Seek(0, io.SeekStart); err == nil {
			if cfg, format, err := image.DecodeConfig(fh); err == nil {
				meta["width"], meta["height"] = cfg.Width, cfg.Height
				return result(fmt.Sprintf("%s is a %s image, %dx%d, %s. Image contents are not returned.", shown, format, cfg.Width, cfg.Height, human(size)), meta), nil
			}
		}
		return result(fmt.Sprintf("%s is an image (%s, %s). Image contents are not returned.", shown, ct, human(size)), meta), nil
	}
	if looksBinary(prefix) {
		ct := http.DetectContentType(prefix)
		meta["type"] = ct
		return result(fmt.Sprintf("%s is a binary file (%s, %s); its contents cannot be read as text.", shown, ct, human(size)), meta), nil
	}

	if a.Offset >= size && size > 0 {
		return integration.Result{}, fmt.Errorf("%s: offset %d is past the end of the file (%d bytes)", shown, a.Offset, size)
	}
	n := f.maxRead
	if a.Limit > 0 && a.Limit < n {
		n = a.Limit
	}
	if rest := size - a.Offset; rest < n {
		n = rest
	}
	buf := make([]byte, n)
	got, err := fh.ReadAt(buf, a.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return integration.Result{}, fmt.Errorf("%s: %s", shown, osErr(err))
	}
	buf = buf[:got]
	end := a.Offset + int64(got)
	more := end < size
	if more { // don't split a UTF-8 sequence at the cut
		for k := 1; k <= 3 && k <= len(buf); k++ {
			if utf8.RuneStart(buf[len(buf)-k]) {
				if !utf8.FullRune(buf[len(buf)-k:]) {
					buf = buf[:len(buf)-k]
					end -= int64(k)
				}
				break
			}
		}
	}
	text := string(bytes.ToValidUTF8(buf, []byte("\uFFFD")))
	meta["offset"], meta["next_offset"] = a.Offset, end
	if more {
		text += fmt.Sprintf("\n[truncated: bytes %d-%d of %d; call again with offset=%d to continue]", a.Offset, end, size, end)
	}
	return result(text, meta), nil
}

func (f *Integration) stat(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	r, rel, err := f.parsePath(a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	shown := display(r, rel)
	real, err := f.existing(r, rel, shown)
	if err != nil {
		return integration.Result{}, err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return integration.Result{}, fmt.Errorf("%s: %s", shown, osErr(err))
	}
	mod := fi.ModTime().UTC().Truncate(time.Second)
	data := map[string]any{"path": shown, "modified": mod, "root_writable": r.writable}
	if fi.IsDir() {
		data["type"] = "folder"
		return result(fmt.Sprintf("%s is a folder, modified %s, root %s.", shown, mod.Format(time.RFC3339), writableWord(r)), data), nil
	}
	if !fi.Mode().IsRegular() {
		return integration.Result{}, pathErr("%q is not a regular file or folder", shown)
	}
	if !f.extOK(real) || !f.extOK(rel) {
		return integration.Result{}, pathErr("%q: only %s files are allowed", shown, strings.Join(f.extList, ", "))
	}
	ct := "application/octet-stream"
	if fh, err := os.Open(real); err == nil {
		buf := make([]byte, sniffLen)
		n, _ := io.ReadFull(fh, buf)
		fh.Close()
		ct = http.DetectContentType(buf[:n])
	}
	data["type"], data["size"], data["content_type"] = "file", fi.Size(), ct
	return result(fmt.Sprintf("%s is a file: %s, %s, modified %s, root %s.", shown, human(fi.Size()), ct, mod.Format(time.RFC3339), writableWord(r)), data), nil
}

func writableWord(r *root) string {
	if r.writable {
		return "writable"
	}
	return "read-only"
}

func checkText(content string) error {
	if len(content) > MaxWriteBytes {
		return fmt.Errorf("content is %s; the limit is %s", human(int64(len(content))), human(MaxWriteBytes))
	}
	if strings.ContainsRune(content, 0) {
		return errors.New("content contains NUL; only text can be written")
	}
	return nil
}

// writeTarget resolves a path for writing and returns the file's resolved
// location and whether it exists (as a regular file inside the root).
func (f *Integration) writeTarget(in string, createDirs bool) (r *root, rel, shown, target string, exists bool, err error) {
	r, rel, err = f.parsePath(in)
	if err != nil {
		return
	}
	shown = display(r, rel)
	if err = r.mustWrite(); err != nil {
		return
	}
	if rel == "." {
		err = pathErr("%q is a root; name a file inside it", shown)
		return
	}
	if err = f.checkExt(rel); err != nil {
		return
	}
	var parent, base string
	if parent, base, err = f.parentOf(r, rel, shown, createDirs); err != nil {
		return
	}
	full := filepath.Join(parent, base)
	if _, lerr := os.Lstat(full); lerr != nil {
		target = full
		return
	}
	var real string
	if real, err = f.existing(r, rel, shown); err != nil {
		return
	}
	if !f.extOK(real) {
		err = pathErr("%q links to a file type that is not allowed", shown)
		return
	}
	fi, serr := os.Stat(real)
	if serr != nil || !fi.Mode().IsRegular() {
		err = pathErr("%q is not a regular file", shown)
		return
	}
	target, exists = real, true
	return
}

func (f *Integration) write(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		Overwrite  bool   `json:"overwrite"`
		CreateDirs bool   `json:"create_dirs"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	if err := checkText(a.Content); err != nil {
		return integration.Result{}, err
	}
	if !f.inited {
		return integration.Result{}, errors.New("files: not initialised")
	}
	_, _, shown, target, exists, err := f.writeTarget(a.Path, a.CreateDirs)
	if err != nil {
		return integration.Result{}, err
	}
	if exists && !a.Overwrite {
		return integration.Result{}, fmt.Errorf("%s already exists; pass overwrite=true to replace it", shown)
	}
	mode := os.FileMode(0o644)
	if exists {
		if fi, err := os.Stat(target); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	if err := atomicWrite(target, []byte(a.Content), mode, exists); err != nil {
		return integration.Result{}, fmt.Errorf("write %s: %s", shown, osErr(err))
	}
	verb := "Created"
	if exists {
		verb = "Replaced"
	}
	return result(fmt.Sprintf("%s %s (%d bytes).", verb, shown, len(a.Content)), map[string]any{"path": shown, "bytes": len(a.Content), "overwritten": exists}), nil
}

// atomicWrite writes data to a temp file beside target and renames it into
// place. Without replace it never clobbers an existing file (hard link then
// unlink, falling back to rename where links are unsupported).
func atomicWrite(target string, data []byte, mode os.FileMode, replace bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".bosun-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if replace {
		if err := os.Rename(name, target); err != nil {
			cleanup()
			return err
		}
		return nil
	}
	if err := os.Link(name, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			cleanup()
			return err
		}
		if _, lerr := os.Lstat(target); lerr == nil {
			cleanup()
			return fs.ErrExist
		}
		if rerr := os.Rename(name, target); rerr != nil {
			cleanup()
			return rerr
		}
		return nil
	}
	cleanup()
	return nil
}

func (f *Integration) append(_ context.Context, in json.RawMessage) (integration.Result, error) {
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
	if err := checkText(a.Content); err != nil {
		return integration.Result{}, err
	}
	if !f.inited {
		return integration.Result{}, errors.New("files: not initialised")
	}
	_, _, shown, target, exists, err := f.writeTarget(a.Path, false)
	if err != nil {
		return integration.Result{}, err
	}
	fh, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return integration.Result{}, fmt.Errorf("append to %s: %s", shown, osErr(err))
	}
	defer fh.Close()
	text := a.Content
	if exists {
		fi, err := fh.Stat()
		if err != nil {
			return integration.Result{}, fmt.Errorf("append to %s: %s", shown, osErr(err))
		}
		if fi.Size()+int64(len(text))+1 > MaxWriteBytes {
			return integration.Result{}, fmt.Errorf("%s would exceed the %s file limit", shown, human(MaxWriteBytes))
		}
		if fi.Size() > 0 {
			last := make([]byte, 1)
			if rf, err := os.Open(target); err == nil {
				_, _ = rf.ReadAt(last, fi.Size()-1)
				rf.Close()
				if last[0] != '\n' {
					text = "\n" + text
				}
			}
		}
	}
	if _, err := fh.WriteString(text); err != nil {
		return integration.Result{}, fmt.Errorf("append to %s: %s", shown, osErr(err))
	}
	return result(fmt.Sprintf("Appended %d bytes to %s.", len(a.Content), shown), map[string]any{"path": shown, "bytes": len(a.Content)}), nil
}

// entryParent resolves the parent of an existing entry (file or folder) and
// Lstat's the entry itself without following it, so move/delete act on the
// directory entry, never on a symlink's target.
func (f *Integration) entryParent(in string) (*root, string, string, string, os.FileInfo, error) {
	r, rel, err := f.parsePath(in)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	shown := display(r, rel)
	if err := r.mustWrite(); err != nil {
		return nil, "", "", "", nil, err
	}
	parent, base, err := f.parentOf(r, rel, shown, false)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	full := filepath.Join(parent, base)
	fi, err := os.Lstat(full)
	if err != nil {
		return nil, "", "", "", nil, fmt.Errorf("%s: %s", shown, osErr(err))
	}
	return r, rel, shown, full, fi, nil
}

func (f *Integration) move(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	rf, relFrom, fromShown, fromFull, fi, err := f.entryParent(a.From)
	if err != nil {
		return integration.Result{}, err
	}
	rt, relTo, err := f.parsePath(a.To)
	if err != nil {
		return integration.Result{}, err
	}
	toShown := display(rt, relTo)
	if rt != rf {
		return integration.Result{}, fmt.Errorf("move must stay within one root (%s -> %s)", rf.name, rt.name)
	}
	if !fi.IsDir() {
		if err := f.checkExt(relFrom); err != nil {
			return integration.Result{}, err
		}
		if err := f.checkExt(relTo); err != nil {
			return integration.Result{}, err
		}
	}
	parent, base, err := f.parentOf(rt, relTo, toShown, false)
	if err != nil {
		return integration.Result{}, err
	}
	toFull := filepath.Join(parent, base)
	if _, err := os.Lstat(toFull); err == nil {
		return integration.Result{}, fmt.Errorf("%s already exists", toShown)
	}
	if err := os.Rename(fromFull, toFull); err != nil {
		return integration.Result{}, fmt.Errorf("move %s to %s: %s", fromShown, toShown, osErr(err))
	}
	return result(fmt.Sprintf("Moved %s to %s.", fromShown, toShown), map[string]any{"from": fromShown, "to": toShown}), nil
}

func (f *Integration) mkdir(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	r, rel, err := f.parsePath(a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	shown := display(r, rel)
	if err := r.mustWrite(); err != nil {
		return integration.Result{}, err
	}
	parent, base, err := f.parentOf(r, rel, shown, true)
	if err != nil {
		return integration.Result{}, err
	}
	full := filepath.Join(parent, base)
	if fi, err := os.Stat(full); err == nil {
		if fi.IsDir() {
			return result(shown+"/ already exists.", map[string]any{"path": shown}), nil
		}
		return integration.Result{}, fmt.Errorf("%s exists and is not a folder", shown)
	}
	if err := os.Mkdir(full, 0o755); err != nil {
		return integration.Result{}, fmt.Errorf("mkdir %s: %s", shown, osErr(err))
	}
	return result("Created folder "+shown+"/.", map[string]any{"path": shown}), nil
}

func (f *Integration) delete(_ context.Context, in json.RawMessage) (integration.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(in, &a); err != nil {
		return integration.Result{}, err
	}
	_, rel, shown, full, fi, err := f.entryParent(a.Path)
	if err != nil {
		return integration.Result{}, err
	}
	if fi.IsDir() {
		return integration.Result{}, fmt.Errorf("%s is a folder; deleting folders is not supported", shown)
	}
	if err := f.checkExt(rel); err != nil {
		return integration.Result{}, err
	}
	if err := os.Remove(full); err != nil {
		return integration.Result{}, fmt.Errorf("delete %s: %s", shown, osErr(err))
	}
	return result("Deleted "+shown+".", map[string]any{"path": shown}), nil
}
