package sandbox

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Request and output limits.
const (
	MaxCodeBytes      = 1 << 20 // code
	MaxStdinBytes     = 1 << 20 // stdin
	MaxFileBytes      = 1 << 20 // one input file
	MaxInputBytes     = 8 << 20 // all input files together
	MaxInputFiles     = 50
	MaxArgs           = 64
	MaxArgBytes       = 4096
	maxNameLen        = 100
	MaxStreamBytes    = 64 << 10 // stdout, stderr each
	MaxOutputBytes    = 8 << 20  // all output files together
	MaxOutputFiles    = 20
	maxInlineFileText = 64 << 10 // text outputs up to this size are shown in Content
)

// request is the sandbox.run argument object.
type request struct {
	Language string            `json:"language"`
	Code     string            `json:"code"`
	Files    map[string]string `json:"files,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Stdin    string            `json:"stdin,omitempty"`
}

// mainFile is the file the code is written to, per language.
var mainFile = map[string]string{
	LangPython: "main.py",
	LangNode:   "main.js",
	LangShell:  "main.sh",
}

// interpreter is the command that runs mainFile, per language.
var interpreter = map[string][]string{
	LangPython: {"python3", "main.py"},
	LangNode:   {"node", "main.js"},
	LangShell:  {"sh", "main.sh"},
}

// validate checks r against the limits and returns the input files with
// sanitized names (sorted by name for a deterministic archive).
func (r *request) validate() ([]inputFile, error) {
	if _, ok := mainFile[r.Language]; !ok {
		return nil, fmt.Errorf("language must be one of %s", strings.Join(languages, ", "))
	}
	if strings.TrimSpace(r.Code) == "" {
		return nil, errors.New("code is required")
	}
	if len(r.Code) > MaxCodeBytes {
		return nil, fmt.Errorf("code is larger than %d bytes", MaxCodeBytes)
	}
	if len(r.Stdin) > MaxStdinBytes {
		return nil, fmt.Errorf("stdin is larger than %d bytes", MaxStdinBytes)
	}
	if len(r.Args) > MaxArgs {
		return nil, fmt.Errorf("at most %d args", MaxArgs)
	}
	for i, a := range r.Args {
		if len(a) > MaxArgBytes {
			return nil, fmt.Errorf("args[%d] is larger than %d bytes", i, MaxArgBytes)
		}
		if strings.IndexByte(a, 0) >= 0 {
			return nil, fmt.Errorf("args[%d] contains a NUL byte", i)
		}
	}
	if len(r.Files) > MaxInputFiles {
		return nil, fmt.Errorf("at most %d input files", MaxInputFiles)
	}
	total := 0
	seen := map[string]string{}
	files := make([]inputFile, 0, len(r.Files))
	for name, content := range r.Files {
		clean := sanitizeName(name)
		if clean == "" {
			return nil, fmt.Errorf("input file name %q is not usable", name)
		}
		if prev, dup := seen[clean]; dup {
			return nil, fmt.Errorf("input files %q and %q both map to in/%s", prev, name, clean)
		}
		seen[clean] = name
		if len(content) > MaxFileBytes {
			return nil, fmt.Errorf("input file %q is larger than %d bytes", name, MaxFileBytes)
		}
		total += len(content)
		if total > MaxInputBytes {
			return nil, fmt.Errorf("input files total more than %d bytes", MaxInputBytes)
		}
		files = append(files, inputFile{Name: clean, Content: content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

type inputFile struct {
	Name    string // sanitized, flat
	Content string
}

// sanitizeName reduces a model-supplied file name to a flat, safe name for
// /work/in/: the last path element, with anything outside [A-Za-z0-9._-]
// replaced by "_", leading dots removed (no hidden files, no "." or ".."),
// and at most 100 bytes. It returns "" if nothing usable remains.
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimRight(name, "/"))
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	out := strings.TrimLeft(sb.String(), ".")
	if len(out) > maxNameLen {
		out = out[:maxNameLen]
	}
	if strings.Trim(out, "_") == "" {
		return ""
	}
	return out
}

// buildInputTar returns the archive streamed to the container's stdin: the
// code as /work/<mainFile>, input files under /work/in/, and the run's stdin
// as /work/.bosun/stdin.
func buildInputTar(r *request, files []inputFile, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755, ModTime: now, Format: tar.FormatPAX})
	}
	file := func(name, content string) error {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: now, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := io.WriteString(tw, content)
		return err
	}
	if err := dir(".bosun/"); err != nil {
		return nil, err
	}
	if err := file(".bosun/stdin", r.Stdin); err != nil {
		return nil, err
	}
	if err := file(mainFile[r.Language], r.Code); err != nil {
		return nil, err
	}
	if err := dir("in/"); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := file("in/"+f.Name, f.Content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OutputFile is one file the code wrote under /work/out/.
type OutputFile struct {
	Size int64 `json:"size"`
	// Encoding is "utf8" (Content is the text) or "base64".
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// Output is sandbox.run's Result.Data.
type Output struct {
	ExitCode        int                   `json:"exit_code"`
	Stdout          string                `json:"stdout"`
	Stderr          string                `json:"stderr"`
	StdoutTruncated bool                  `json:"stdout_truncated,omitempty"`
	StderrTruncated bool                  `json:"stderr_truncated,omitempty"`
	Files           map[string]OutputFile `json:"files"`
	// Omitted names output files that were not collected (over the file
	// count or size cap, or not regular files).
	Omitted    []string `json:"omitted,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Image      string   `json:"image"`
}

// errNoExit means the result archive did not contain the exit status, i.e.
// the wrapper never finished (docker failed, or the container was killed).
var errNoExit = errors.New("sandbox: run produced no result")

// parseOutputTar reads the archive the wrapper writes to stdout, applying
// the stream and file caps. Anything beyond a cap is read and discarded.
func parseOutputTar(r io.Reader) (*Output, error) {
	out := &Output{Files: map[string]OutputFile{}}
	tr := tar.NewReader(r)
	gotExit := false
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if gotExit {
				break // a truncated tail only loses output files
			}
			return out, fmt.Errorf("%w: %v", errNoExit, err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		switch name {
		case ".bosun/exit":
			b, _ := io.ReadAll(io.LimitReader(tr, 32))
			var code int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &code); err == nil {
				out.ExitCode = code
				gotExit = true
			}
			continue
		case ".bosun/stdout":
			out.Stdout, out.StdoutTruncated = readCapped(tr, MaxStreamBytes)
			continue
		case ".bosun/stderr":
			out.Stderr, out.StderrTruncated = readCapped(tr, MaxStreamBytes)
			continue
		}
		rel, ok := strings.CutPrefix(name, "out/")
		if !ok || rel == "" || h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			out.Omitted = append(out.Omitted, rel)
			continue
		}
		if len(out.Files) >= MaxOutputFiles || total+h.Size > MaxOutputBytes {
			out.Omitted = append(out.Omitted, rel)
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			out.Omitted = append(out.Omitted, rel)
			continue
		}
		total += int64(len(b))
		f := OutputFile{Size: int64(len(b))}
		if isText(b) {
			f.Encoding, f.Content = "utf8", string(b)
		} else {
			f.Encoding, f.Content = "base64", base64.StdEncoding.EncodeToString(b)
		}
		out.Files[rel] = f
	}
	if !gotExit {
		return out, errNoExit
	}
	return out, nil
}

func readCapped(r io.Reader, n int64) (string, bool) {
	b, _ := io.ReadAll(io.LimitReader(r, n+1))
	if int64(len(b)) > n {
		b = b[:n]
		// do not leave a split UTF-8 sequence at the end
		for i := 0; i < utf8.UTFMax-1 && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
		return strings.ToValidUTF8(string(b), "�"), true
	}
	return strings.ToValidUTF8(string(b), "�"), false
}

func isText(b []byte) bool { return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 }

// report renders the compact text handed to the model.
func (o *Output) report() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit code: %d (%.1fs, %s)\n", o.ExitCode, float64(o.DurationMS)/1000, o.Image)
	stream := func(label, s string, trunc bool) {
		if s == "" {
			fmt.Fprintf(&sb, "%s: (empty)\n", label)
			return
		}
		note := ""
		if trunc {
			note = fmt.Sprintf(" (truncated to %d bytes)", MaxStreamBytes)
		}
		fmt.Fprintf(&sb, "--- %s%s ---\n%s", label, note, s)
		if !strings.HasSuffix(s, "\n") {
			sb.WriteByte('\n')
		}
	}
	stream("stdout", o.Stdout, o.StdoutTruncated)
	stream("stderr", o.Stderr, o.StderrTruncated)
	names := make([]string, 0, len(o.Files))
	for n := range o.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 && len(o.Omitted) == 0 {
		sb.WriteString("output files: none\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "--- output files (%d) ---\n", len(names))
	for _, n := range names {
		f := o.Files[n]
		if f.Encoding == "utf8" && f.Size <= maxInlineFileText {
			fmt.Fprintf(&sb, "out/%s (%d bytes):\n%s", n, f.Size, f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				sb.WriteByte('\n')
			}
			continue
		}
		kind := "binary"
		if f.Encoding == "utf8" {
			kind = "text, not shown"
		}
		fmt.Fprintf(&sb, "out/%s (%d bytes, %s)\n", n, f.Size, kind)
	}
	if len(o.Omitted) > 0 {
		fmt.Fprintf(&sb, "not collected (limit %d files / %d bytes, or not a regular file): %s\n",
			MaxOutputFiles, MaxOutputBytes, strings.Join(o.Omitted, ", "))
	}
	return sb.String()
}
