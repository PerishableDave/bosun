package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Keychain is an OS secret store holding one string secret per (service,
// account): the macOS Keychain via the `security` CLI, or the freedesktop
// Secret Service via libsecret's `secret-tool`. Implementations shell out
// (no cgo) through a CommandRunner, so tests substitute a fake and never
// touch the real keychain.
type Keychain interface {
	// Name describes the backend for logs, e.g. "macOS Keychain".
	Name() string
	// Get returns the secret, or ErrKeychainItemNotFound.
	Get(ctx context.Context, service, account string) (string, error)
	// Set creates or replaces the secret.
	Set(ctx context.Context, service, account, secret string) error
	// Delete removes the secret, or returns ErrKeychainItemNotFound.
	Delete(ctx context.Context, service, account string) error
}

// ErrKeychainItemNotFound means the keychain has no item for (service,
// account).
var ErrKeychainItemNotFound = errors.New("vault: keychain item not found")

// CommandRunner runs external commands. ExecRunner is the real one.
type CommandRunner interface {
	LookPath(file string) (string, error)
	// Run runs name with args, feeding stdin, and returns stdout. A non-zero
	// exit is reported as *ExitError.
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ExitError is a command that ran and exited non-zero.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, msg)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

// LookPath implements CommandRunner.
func (ExecRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.Bytes(), &ExitError{Code: ee.ExitCode(), Stderr: stderr.String()}
	}
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

// safeToken limits what SecurityCLI passes through `security -i`, whose
// command line is tokenised on whitespace and quotes.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9+/=._:-]+$`)

// SecurityCLI is the macOS Keychain, driven by /usr/bin/security.
type SecurityCLI struct {
	Runner CommandRunner
	// Path is the security binary (default "security" on $PATH).
	Path string
}

func (s *SecurityCLI) bin() string {
	if s.Path != "" {
		return s.Path
	}
	return "security"
}

// securityNotFound is security(1)'s exit status for errSecItemNotFound.
const securityNotFound = 44

// Name implements Keychain.
func (s *SecurityCLI) Name() string { return "macOS Keychain" }

// Get implements Keychain.
func (s *SecurityCLI) Get(ctx context.Context, service, account string) (string, error) {
	out, err := s.Runner.Run(ctx, nil, s.bin(), "find-generic-password", "-s", service, "-a", account, "-w")
	if exitCode(err) == securityNotFound {
		return "", ErrKeychainItemNotFound
	}
	if err != nil {
		return "", fmt.Errorf("security find-generic-password: %w", err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// Set implements Keychain. The secret is passed on stdin to `security -i`
// rather than on argv, where any local user could read it from the process
// table.
func (s *SecurityCLI) Set(ctx context.Context, service, account, secret string) error {
	for _, v := range []string{service, account, secret} {
		if !safeToken.MatchString(v) {
			return errors.New("security add-generic-password: service, account and secret must be [A-Za-z0-9+/=._:-]")
		}
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -w %s\n", service, account, service, secret)
	if _, err := s.Runner.Run(ctx, []byte(line), s.bin(), "-i"); err != nil {
		return fmt.Errorf("security add-generic-password: %w", err)
	}
	return nil
}

// Delete implements Keychain.
func (s *SecurityCLI) Delete(ctx context.Context, service, account string) error {
	_, err := s.Runner.Run(ctx, nil, s.bin(), "delete-generic-password", "-s", service, "-a", account)
	if exitCode(err) == securityNotFound {
		return ErrKeychainItemNotFound
	}
	if err != nil {
		return fmt.Errorf("security delete-generic-password: %w", err)
	}
	return nil
}

// SecretTool is the freedesktop Secret Service (GNOME Keyring, KWallet, ...)
// driven by libsecret's secret-tool.
type SecretTool struct {
	Runner CommandRunner
	// Path is the secret-tool binary (default "secret-tool" on $PATH).
	Path string
}

func (s *SecretTool) bin() string {
	if s.Path != "" {
		return s.Path
	}
	return "secret-tool"
}

// Name implements Keychain.
func (s *SecretTool) Name() string { return "Secret Service (secret-tool)" }

// Get implements Keychain. secret-tool exits 1 with no output when nothing
// matches.
func (s *SecretTool) Get(ctx context.Context, service, account string) (string, error) {
	out, err := s.Runner.Run(ctx, nil, s.bin(), "lookup", "service", service, "account", account)
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == 1 && len(out) == 0 && strings.TrimSpace(ee.Stderr) == "" {
		return "", ErrKeychainItemNotFound
	}
	if err != nil {
		return "", fmt.Errorf("secret-tool lookup: %w", err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

// Set implements Keychain. secret-tool reads the secret from stdin.
func (s *SecretTool) Set(ctx context.Context, service, account, secret string) error {
	if _, err := s.Runner.Run(ctx, []byte(secret), s.bin(), "store", "--label="+service+" "+account,
		"service", service, "account", account); err != nil {
		return fmt.Errorf("secret-tool store: %w", err)
	}
	return nil
}

// Delete implements Keychain. secret-tool clear does not report whether
// anything matched, so Delete looks the item up first.
func (s *SecretTool) Delete(ctx context.Context, service, account string) error {
	if _, err := s.Get(ctx, service, account); err != nil {
		return err
	}
	if _, err := s.Runner.Run(ctx, nil, s.bin(), "clear", "service", service, "account", account); err != nil {
		return fmt.Errorf("secret-tool clear: %w", err)
	}
	return nil
}

// DetectKeychain returns the OS keychain backend for this platform if its
// tool is installed: SecurityCLI on macOS, SecretTool on Linux; otherwise
// nil. It does not check that the keychain is unlocked or reachable (see
// SelectKeySource).
func DetectKeychain(r CommandRunner) Keychain {
	switch runtime.GOOS {
	case "darwin":
		if p, err := r.LookPath("security"); err == nil {
			return &SecurityCLI{Runner: r, Path: p}
		}
	case "linux", "freebsd", "openbsd", "netbsd":
		if p, err := r.LookPath("secret-tool"); err == nil {
			return &SecretTool{Runner: r, Path: p}
		}
	}
	return nil
}

// Keychain item identity for the master key.
const (
	DefaultKeychainService = "bosun"
	DefaultKeychainAccount = "master-key"
)

// KeychainKeySource keeps the master key (base64) in an OS keychain. On
// first use, if the keychain has no key and ImportFile names an existing key
// file, that key is imported (the file is left in place); otherwise a new
// random key is generated and stored. The key is cached after the first
// read.
type KeychainKeySource struct {
	Keychain Keychain
	Service  string // default DefaultKeychainService
	Account  string // default DefaultKeychainAccount
	// ImportFile is a legacy master.key to import when the keychain is empty.
	ImportFile string
	Log        *slog.Logger

	mu  sync.Mutex
	key []byte
}

func (k *KeychainKeySource) ids() (string, string) {
	svc, acct := k.Service, k.Account
	if svc == "" {
		svc = DefaultKeychainService
	}
	if acct == "" {
		acct = DefaultKeychainAccount
	}
	return svc, acct
}

func (k *KeychainKeySource) logger() *slog.Logger {
	if k.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return k.Log
}

// Key implements KeySource.
func (k *KeychainKeySource) Key(ctx context.Context) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != nil {
		return k.key, nil
	}
	svc, acct := k.ids()
	s, err := k.Keychain.Get(ctx, svc, acct)
	if err == nil {
		key, err := decodeKey(s)
		if err != nil {
			return nil, fmt.Errorf("vault: %s item %s/%s: %w", k.Keychain.Name(), svc, acct, err)
		}
		k.key = key
		return key, nil
	}
	if !errors.Is(err, ErrKeychainItemNotFound) {
		return nil, fmt.Errorf("vault: read master key from %s: %w", k.Keychain.Name(), err)
	}

	var key []byte
	imported := false
	if k.ImportFile != "" {
		if _, statErr := os.Stat(k.ImportFile); statErr == nil {
			fk := &FileKeySource{Path: k.ImportFile}
			if key, err = fk.load(); err != nil {
				return nil, fmt.Errorf("vault: import master key into %s: %w", k.Keychain.Name(), err)
			}
			imported = true
		}
	}
	if key == nil {
		key = make([]byte, KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("vault: generate master key: %w", err)
		}
	}
	enc := base64.StdEncoding.EncodeToString(key)
	if err := k.Keychain.Set(ctx, svc, acct, enc); err != nil {
		return nil, fmt.Errorf("vault: store master key in %s: %w", k.Keychain.Name(), err)
	}
	// Read it back: `security -i` does not reliably report failures of the
	// command it runs, and a key we cannot read back is worse than none.
	if got, err := k.Keychain.Get(ctx, svc, acct); err != nil || got != enc {
		return nil, fmt.Errorf("vault: master key did not round-trip through %s (err %v)", k.Keychain.Name(), err)
	}
	if imported {
		k.logger().Warn("imported master key file into the OS keychain; the file was left in place. "+
			"Delete it yourself once the keychain copy is confirmed (anyone who can read it can decrypt the vault)",
			"keychain", k.Keychain.Name(), "file", k.ImportFile)
	} else {
		k.logger().Info("generated a new master key in the OS keychain", "keychain", k.Keychain.Name())
	}
	k.key = key
	return key, nil
}

func decodeKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("master key is not base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// Master key source modes (config daemon.master_key).
const (
	KeyModeAuto     = "auto"
	KeyModeFile     = "file"
	KeyModeKeychain = "keychain"
)

// KeySourceMarkerFile records, in the data dir, that the master key lives in
// the OS keychain. In auto mode a keychain that later becomes unreachable is
// then an error rather than a silent switch to a brand-new key file (which
// could not decrypt existing credentials).
const KeySourceMarkerFile = "master-key-source"

// KeySourceOptions configure SelectKeySource.
type KeySourceOptions struct {
	// Mode is "auto" (or ""), "file" or "keychain".
	Mode    string
	DataDir string
	// Detect returns the platform keychain or nil (default
	// DetectKeychain(ExecRunner{})). Tests pass a fake.
	Detect func() Keychain
	// Service and Account override the keychain item identity.
	Service, Account string
	Log              *slog.Logger
}

// SelectKeySource picks the master key source for mode and loads the key once
// so problems surface at startup. It returns the source and a short
// description for the startup log.
//
//   - file: $DataDir/master.key (created if missing).
//   - keychain: the OS keychain; an error if none is available. An existing
//     master.key is imported on first use and left in place.
//   - auto: keychain if one is installed and answers, else the file. If the
//     data dir records that the key lives in the keychain, an unreachable
//     keychain is an error instead of a fallback.
func SelectKeySource(ctx context.Context, o KeySourceOptions) (KeySource, string, error) {
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mode := o.Mode
	if mode == "" {
		mode = KeyModeAuto
	}
	detect := o.Detect
	if detect == nil {
		detect = func() Keychain { return DetectKeychain(ExecRunner{}) }
	}
	file := NewFileKeySource(o.DataDir)
	marker := filepath.Join(o.DataDir, KeySourceMarkerFile)
	markedKeychain := false
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == KeyModeKeychain {
		markedKeychain = true
	}

	useFile := func(why string) (KeySource, string, error) {
		if markedKeychain {
			log.Warn("data dir records that the master key is in the OS keychain, but the key file is in use; "+
				"credentials stored under the keychain key cannot be decrypted unless the file holds the same key", "file", file.Path)
		}
		if _, err := file.Key(ctx); err != nil {
			return nil, "", err
		}
		return file, "file " + file.Path + why, nil
	}

	switch mode {
	case KeyModeFile:
		return useFile("")
	case KeyModeKeychain, KeyModeAuto:
	default:
		return nil, "", fmt.Errorf("vault: unknown master key mode %q (want auto, file, or keychain)", mode)
	}

	kc := detect()
	if kc == nil {
		if mode == KeyModeKeychain {
			return nil, "", errors.New("vault: daemon.master_key is keychain but no OS keychain is available " +
				"(macOS `security` or Linux `secret-tool`)")
		}
		if markedKeychain {
			return nil, "", errors.New("vault: the master key is stored in the OS keychain but no keychain tool is available; " +
				"set daemon.master_key: file only if master.key holds the same key")
		}
		return useFile(" (no OS keychain available)")
	}
	ks := &KeychainKeySource{Keychain: kc, Service: o.Service, Account: o.Account, ImportFile: file.Path, Log: log}
	svc, acct := ks.ids()
	if _, err := kc.Get(ctx, svc, acct); err != nil && !errors.Is(err, ErrKeychainItemNotFound) {
		if mode == KeyModeKeychain || markedKeychain {
			return nil, "", fmt.Errorf("vault: %s is unavailable: %w", kc.Name(), err)
		}
		log.Warn("OS keychain is unavailable; falling back to the master key file", "keychain", kc.Name(), "err", err)
		return useFile(" (keychain unavailable)")
	}
	if _, err := ks.Key(ctx); err != nil {
		return nil, "", err
	}
	if !markedKeychain {
		if err := os.WriteFile(marker, []byte(KeyModeKeychain+"\n"), 0o600); err != nil {
			log.Warn("could not record the master key source", "file", marker, "err", err)
		}
	}
	if _, err := os.Stat(file.Path); err == nil {
		// Deliberately not deleted automatically (a failed keychain would
		// otherwise lose the only copy). `bosun doctor` warns about it.
		// TODO: `bosun setup` should offer to delete the imported key file
		// once the keychain copy is confirmed.
		log.Info("master key file is still present; the keychain copy is in use", "file", file.Path)
	}
	return ks, kc.Name() + " (service " + svc + ", account " + acct + ")", nil
}
