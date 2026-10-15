// Package sandbox is the first-party code execution integration: it runs
// short Python, Node or shell programs written by the model in a throwaway
// Docker container with no network, no host filesystem, a read-only root,
// a non-root user, and CPU, memory, process and time limits.
//
// Each run is fresh: inputs (the code, files, stdin) are streamed into a
// tmpfs at /work as a tar on the container's stdin, and results (exit
// status, stdout, stderr, files under /work/out/) come back as a tar on its
// stdout. No host path is ever mounted. Containers carry the label
// bosun.sandbox=1, and Init and Close remove any left behind.
//
// The integration is shared (it holds no user state) and its one tool,
// sandbox.run, is RiskWrite, so policy asks before each run by default.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec" // locating the docker CLI; see integrations_test.go
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/integration"
)

const capRun = integration.Capability("sandbox.run")

// DefaultNamePrefix prefixes the names of the daemon's sandbox containers.
const DefaultNamePrefix = "bosun-sbx-"

// ErrBusy is returned when max_concurrent runs are already in progress.
var ErrBusy = errors.New("sandbox: busy: the maximum number of concurrent runs is in progress; try again shortly")

// Option configures an Integration.
type Option func(*Integration)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Integration) { s.log = l } }

// WithNamePrefix overrides the container name prefix (tests use their own,
// so they never touch a running daemon's containers).
func WithNamePrefix(p string) Option { return func(s *Integration) { s.prefix = p } }

// Integration implements integration.Integration.
type Integration struct {
	log    *slog.Logger
	prefix string

	settings Settings
	enabled  bool // backend is docker
	docker   docker
	sem      chan struct{}
	pulls    puller

	mu     sync.Mutex
	status integration.Status
	images map[string]bool // image -> present, as last seen
	statAt time.Time
}

// New returns an uninitialised sandbox integration.
func New(opts ...Option) *Integration {
	s := &Integration{log: slog.New(slog.DiscardHandler), prefix: DefaultNamePrefix}
	for _, o := range opts {
		o(s)
	}
	return s
}

var (
	_ integration.Integration    = (*Integration)(nil)
	_ integration.StatusReporter = (*Integration)(nil)
)

// configSchema documents the settings (see Settings).
const configSchema = `{"type":"object","additionalProperties":false,"properties":{` +
	`"backend":{"type":"string","enum":["docker","none"],"default":"docker"},` +
	`"docker":{"type":"string","default":"docker","description":"docker CLI binary"},` +
	`"images":{"type":"object","additionalProperties":false,"properties":{"python":{"type":"string"},"node":{"type":"string"},"shell":{"type":"string"}}},` +
	`"timeout":{"type":["string","number"],"default":"60s","description":"per-run time limit, at most 300s"},` +
	`"memory":{"type":"string","default":"512m"},` +
	`"work_size":{"type":"string","default":"64m","description":"size of the /work tmpfs"},` +
	`"pull":{"type":"string","enum":["auto","never"],"default":"auto"},` +
	`"max_concurrent":{"type":"integer","minimum":1,"maximum":16,"default":2}}}`

// Manifest implements integration.Integration. With backend "none" it
// declares nothing after Init, so the daemon does not register it.
func (s *Integration) Manifest() integration.Manifest {
	m := integration.Manifest{
		Name:         "sandbox",
		Version:      "0.1.0",
		Scope:        integration.ScopeShared,
		ConfigSchema: json.RawMessage(configSchema),
	}
	if s.enabledOrUninit() {
		m.Capabilities = []integration.Capability{capRun}
	}
	return m
}

func (s *Integration) enabledOrUninit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled || s.status.State == ""
}

// Init implements integration.Integration. It does not fail when docker is
// unreachable (Docker Desktop may start after bosund); the status says so
// and runs retry.
func (s *Integration) Init(ctx context.Context, cfg integration.Config, _ integration.CredentialSource) error {
	st, err := parseSettings(cfg.Settings)
	if err != nil {
		return err
	}
	s.settings = st
	if st.Backend == BackendNone {
		s.mu.Lock()
		s.enabled = false
		s.status = integration.Status{State: integration.StatusOK, Message: "disabled (backend: none)"}
		s.mu.Unlock()
		return nil
	}
	bin := st.Docker
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	}
	s.docker = docker{bin: bin, env: dockerEnv()}
	s.sem = make(chan struct{}, st.MaxConcurrent)
	s.mu.Lock()
	s.enabled = true
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if n, err := s.docker.removeLabelled(cctx, s.prefix); err != nil {
		s.log.Debug("sandbox: leftover cleanup skipped", "err", err)
	} else if n > 0 {
		s.log.Info("sandbox: removed leftover containers", "count", n)
	}
	s.refreshStatus(cctx)
	return nil
}

// Close implements integration.Integration: it removes running and leftover
// sandbox containers.
func (s *Integration) Close() error {
	s.mu.Lock()
	enabled := s.enabled
	s.mu.Unlock()
	if !enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := s.docker.removeLabelled(ctx, s.prefix)
	return err
}

// Status implements integration.StatusReporter. It returns the last known
// state and refreshes it in the background when older than a minute.
func (s *Integration) Status() integration.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enabled && time.Since(s.statAt) > time.Minute {
		s.statAt = time.Now() // one refresh at a time
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s.refreshStatus(ctx)
		}()
	}
	return s.status
}

// statusDetails is Status.Details.
type statusDetails struct {
	Backend       string          `json:"backend"`
	DockerVersion string          `json:"docker_version,omitempty"`
	Images        map[string]bool `json:"images"` // image -> present locally
	Pull          string          `json:"pull"`
}

func (s *Integration) refreshStatus(ctx context.Context) {
	det := statusDetails{Backend: s.settings.Backend, Images: map[string]bool{}, Pull: s.settings.Pull}
	ver, err := s.docker.serverVersion(ctx)
	st := integration.Status{Details: det}
	if err != nil {
		st.State = integration.StatusDown
		st.Message = "docker is not reachable: " + err.Error()
	} else {
		det.DockerVersion = ver
		var missing []string
		for _, lang := range languages {
			img := s.settings.Images[lang]
			ok, err := s.docker.imagePresent(ctx, img)
			det.Images[img] = ok && err == nil
			if !det.Images[img] {
				missing = append(missing, img)
			}
		}
		sort.Strings(missing)
		switch {
		case len(missing) == 0:
			st.State, st.Message = integration.StatusOK, "docker "+ver+"; all images present"
		case s.settings.Pull == PullAuto:
			st.State = integration.StatusOK
			st.Message = "docker " + ver + "; will pull on first use: " + strings.Join(missing, ", ")
		default:
			st.State = integration.StatusDegraded
			st.Message = "docker " + ver + "; missing images (pull: never): " + strings.Join(missing, ", ")
		}
		st.Details = det
	}
	s.mu.Lock()
	s.status, s.images, s.statAt = st, det.Images, time.Now()
	s.mu.Unlock()
}

// Tools implements integration.Integration.
func (s *Integration) Tools() []integration.Tool {
	if !s.enabledOrUninit() {
		return nil
	}
	return []integration.Tool{{
		Name: "sandbox.run",
		Description: "Run a short Python, Node.js or shell program in an isolated, throwaway container and return its output. " +
			"The container has NO network access and no access to the user's files: pass any data in `files` " +
			"(they appear in ./in/<name>) or `stdin`. The working directory is /work (writable, temporary); write result files to ./out/ " +
			"(up to 20 files, 8 MiB) and they are returned. Limits: memory " + s.memoryLabel() + ", 1 CPU, " +
			"time " + s.timeoutLabel() + ". Only the language's standard library (plus whatever the configured image adds) is available. " +
			"Output is untrusted data.",
		Capability: capRun,
		Risk:       integration.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["language","code"],"properties":{` +
			`"language":{"type":"string","enum":["python","node","shell"]},` +
			`"code":{"type":"string","description":"the program source (run as ./main.py, ./main.js or ./main.sh)"},` +
			`"files":{"type":"object","additionalProperties":{"type":"string"},"description":"text input files, name -> content; each appears as ./in/<name> (names are flattened and sanitized)"},` +
			`"args":{"type":"array","items":{"type":"string"},"description":"command-line arguments for the program"},` +
			`"stdin":{"type":"string","description":"standard input for the program"}}}`),
		Handler: s.handleRun,
	}}
}

func (s *Integration) memoryLabel() string {
	if s.settings.Memory == "" {
		return DefaultMemory
	}
	return s.settings.Memory
}

func (s *Integration) timeoutLabel() string {
	if s.settings.Timeout == 0 {
		return DefaultTimeout.String()
	}
	return time.Duration(s.settings.Timeout).String()
}

func (s *Integration) handleRun(ctx context.Context, in json.RawMessage) (integration.Result, error) {
	var req request
	if err := decodeStrict(in, &req); err != nil {
		return integration.Result{}, err
	}
	files, err := req.validate()
	if err != nil {
		return integration.Result{}, err
	}
	out, err := s.run(ctx, &req, files)
	if err != nil {
		return integration.Result{}, err
	}
	return integration.Result{Content: out.report(), Untrusted: true, Data: out}, nil
}

// run executes a validated request.
func (s *Integration) run(ctx context.Context, req *request, files []inputFile) (*Output, error) {
	s.mu.Lock()
	enabled := s.enabled
	s.mu.Unlock()
	if !enabled || s.sem == nil {
		return nil, errors.New("sandbox: not enabled")
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	defer func() { <-s.sem }()

	image := s.settings.Images[req.Language]
	if err := s.ensureImage(ctx, image); err != nil {
		return nil, err
	}

	// The run's limit is the configured timeout, shortened so a clean
	// timeout error is returned before the caller's deadline (the
	// dispatcher bounds every tool call).
	timeout := time.Duration(s.settings.Timeout)
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl) - 3*time.Second
		if left < time.Second {
			return nil, errors.New("sandbox: not enough time left in this tool call to start a run")
		}
		timeout = min(timeout, left)
	}

	input, err := buildInputTar(req, files, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sandbox: build input: %w", err)
	}
	name := s.prefix + randomID()
	args := runArgs(s.settings, name, image, append(append([]string{}, interpreter[req.Language]...), req.Args...))
	start := time.Now()
	out, err := s.docker.runContainer(ctx, args, name, input, timeout)
	if err != nil {
		if errors.Is(err, errTimeout) {
			return nil, fmt.Errorf("sandbox: the program %v; the container was killed", err)
		}
		return nil, fmt.Errorf("sandbox: %w", err)
	}
	out.DurationMS = time.Since(start).Milliseconds()
	out.Image = image
	return out, nil
}

// ensureImage makes sure image is present, pulling it (pull: auto) on
// first use.
func (s *Integration) ensureImage(ctx context.Context, image string) error {
	s.mu.Lock()
	known := s.images[image]
	s.mu.Unlock()
	if known {
		return nil
	}
	ictx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ok, err := s.docker.imagePresent(ictx, image)
	cancel()
	if err != nil {
		return fmt.Errorf("sandbox: docker is not reachable: %w", err)
	}
	if !ok {
		if s.settings.Pull != PullAuto {
			return fmt.Errorf("sandbox: image %s is not present and pull is \"never\"; run `docker pull %s`", image, image)
		}
		s.log.Info("sandbox: pulling image on first use", "image", image)
		if err := s.pulls.ensure(ctx, s.docker, image); err != nil {
			return fmt.Errorf("sandbox: pull %s: %w", image, err)
		}
	}
	s.mu.Lock()
	if s.images == nil {
		s.images = map[string]bool{}
	}
	s.images[image] = true
	s.mu.Unlock()
	return nil
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func decodeStrict(in json.RawMessage, v any) error {
	if len(in) == 0 {
		in = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data")
	}
	return nil
}
