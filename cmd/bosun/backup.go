package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/config"
	"github.com/perishabledave/bosun/internal/store"
	"github.com/perishabledave/bosun/internal/vault"
)

// Backup archive layout (tar.gz, mode 0600):
//
//	bosun-backup.json   manifest (always the first entry)
//	bosun.yaml          the config file
//	data/bosun.db       a consistent snapshot (VACUUM INTO), not a raw copy
//	data/...            everything else in the data dir (master.key, env,
//	                    integrations/, tls/, ...) except the live database
//	                    files, the CLI session token, and logs/
const (
	backupManifestName = "bosun-backup.json"
	backupConfigName   = "bosun.yaml"
	backupDataPrefix   = "data/"
	backupFormat       = 1
	maxBackupEntry     = 8 << 30
)

// Master key states recorded in the manifest.
const (
	keyInArchive   = "file"                  // master.key from the data dir
	keyExported    = "keychain-exported"     // exported from the OS keychain
	keyNotIncluded = "keychain-not-included" // in the keychain, not in the archive
	keyNone        = "none"                  // no key yet
)

// backupManifest is bosun-backup.json.
type backupManifest struct {
	Format        int       `json:"format"`
	CreatedAt     time.Time `json:"created_at"`
	BosunVersion  string    `json:"bosun_version"`
	SchemaVersion int       `json:"schema_version"`
	DataDir       string    `json:"data_dir"`
	MasterKey     string    `json:"master_key"`
}

// backupExcluded reports whether a data-dir relative path is left out of a
// backup: the live database (a snapshot is taken instead), CLI session
// state, logs, and temp files.
func backupExcluded(rel string) bool {
	switch rel {
	case "bosun.db", "bosun.db-wal", "bosun.db-shm", "bosun.db-journal", "cli-token", ServerFileName, "logs":
		return true
	}
	return strings.HasPrefix(rel, "logs/") || strings.HasSuffix(rel, ".tmp")
}

func newBackupCmd(g *globals) *cobra.Command {
	var out string
	var includeKC bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a backup of the config, database, vault key and integration state",
		Long: `backup writes a .tar.gz (mode 0600) with bosun.yaml, a consistent snapshot of
the database (safe while bosund runs), and the data dir's files: master.key
(when the vault key is file-based), env, integrations/ and tls/. The CLI
session token and logs are left out.

If the vault master key lives in the OS keychain, it is NOT included unless
you pass --include-keychain-key; without it, credentials in the backup can
only be decrypted on a machine whose keychain holds the same key. Anyone
with an archive that contains the key can read every stored credential:
keep it like a password.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.loadConfig()
			if err != nil {
				return err
			}
			if out == "" {
				out = "bosun-backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
			}
			_, err = runBackup(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), g.configPath, cfg, out, includeKC)
			return err
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "archive path (default bosun-backup-<time>.tar.gz)")
	cmd.Flags().BoolVar(&includeKC, "include-keychain-key", false, "export the vault master key from the OS keychain into the archive")
	return cmd
}

// keychainInUse reports whether the data dir records that the master key
// lives in the OS keychain (or config forces it).
func keychainInUse(cfg *config.Config) bool {
	if cfg.Daemon.MasterKey == vault.KeyModeKeychain {
		return true
	}
	if cfg.Daemon.MasterKey == vault.KeyModeFile {
		return false
	}
	b, err := os.ReadFile(filepath.Join(cfg.Daemon.DataDir, vault.KeySourceMarkerFile))
	return err == nil && strings.TrimSpace(string(b)) == vault.KeyModeKeychain
}

func runBackup(ctx context.Context, out, warnOut io.Writer, cfgPath string, cfg *config.Config, archive string, includeKC bool) (*backupManifest, error) {
	dataDir := cfg.Daemon.DataDir
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		return nil, fmt.Errorf("no database at %s (nothing to back up): %w", cfg.DBPath(), err)
	}
	m := &backupManifest{Format: backupFormat, CreatedAt: time.Now().UTC(), BosunVersion: version, DataDir: dataDir, MasterKey: keyNone}

	// The vault master key.
	var exportedKey []byte
	_, keyFileErr := os.Stat(filepath.Join(dataDir, vault.MasterKeyFile))
	switch {
	case keychainInUse(cfg) && includeKC:
		kc := detectKeychain()
		if kc == nil {
			return nil, errors.New("--include-keychain-key: no OS keychain tool is available")
		}
		s, err := kc.Get(ctx, vault.DefaultKeychainService, vault.DefaultKeychainAccount)
		if err != nil {
			return nil, fmt.Errorf("read the master key from %s: %w", kc.Name(), err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
		if err != nil || len(key) != vault.KeySize {
			return nil, fmt.Errorf("the master key in %s is malformed", kc.Name())
		}
		exportedKey, m.MasterKey = key, keyExported
	case keychainInUse(cfg):
		m.MasterKey = keyNotIncluded
		fmt.Fprint(warnOut, `
WARNING: the vault master key is in the OS keychain and is NOT in this backup.
Without it, the stored credentials in this archive (API keys, mail passwords,
OAuth tokens) cannot be decrypted on another machine or after the keychain
item is lost. Re-run with --include-keychain-key to export the key into the
archive, and then guard the archive like a password.
`)
	case keyFileErr == nil:
		m.MasterKey = keyInArchive
	}

	// A consistent database snapshot.
	tmpDir, err := os.MkdirTemp("", "bosun-backup-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	snap := filepath.Join(tmpDir, "bosun.db")
	st, err := store.Open(ctx, cfg.DBPath())
	if err != nil {
		return nil, err
	}
	err = st.BackupTo(ctx, snap)
	st.Close()
	if err != nil {
		return nil, err
	}
	sst, err := store.Open(ctx, snap)
	if err != nil {
		return nil, err
	}
	m.SchemaVersion, err = sst.SchemaVersion(ctx)
	sst.Close()
	if err != nil {
		return nil, err
	}
	// Opening the snapshot may have left side files; the db file is what we
	// archive, and it is complete once closed.

	f, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", archive, err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(archive)
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	now := time.Now()
	addBytes := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	addFile := func(name, src string) error {
		sf, err := os.Open(src)
		if err != nil {
			return err
		}
		defer sf.Close()
		fi, err := sf.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = io.Copy(tw, sf)
		return err
	}

	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := addBytes(backupManifestName, mb); err != nil {
		return nil, err
	}
	if err := addFile(backupConfigName, cfgPath); err != nil {
		return nil, fmt.Errorf("add config: %w", err)
	}
	if err := addFile(backupDataPrefix+"bosun.db", snap); err != nil {
		return nil, fmt.Errorf("add database: %w", err)
	}
	if exportedKey != nil {
		if err := addBytes(backupDataPrefix+vault.MasterKeyFile, exportedKey); err != nil {
			return nil, err
		}
	}
	cfgAbs, _ := filepath.Abs(cfgPath)
	files := 0
	err = filepath.WalkDir(dataDir, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if backupExcluded(rel) || (exportedKey != nil && rel == vault.MasterKeyFile) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if abs, _ := filepath.Abs(p); abs == cfgAbs {
			return nil // already archived as bosun.yaml
		}
		switch {
		case de.IsDir():
			return tw.WriteHeader(&tar.Header{Name: backupDataPrefix + rel + "/", Mode: 0o700, Typeflag: tar.TypeDir, ModTime: now})
		case de.Type().IsRegular():
			files++
			return addFile(backupDataPrefix+rel, p)
		default:
			fmt.Fprintf(warnOut, "skipping %s: not a regular file\n", p)
			return nil
		}
	})
	if err != nil {
		return nil, fmt.Errorf("archive data dir: %w", err)
	}
	if err := errors.Join(tw.Close(), gz.Close(), f.Sync(), f.Close()); err != nil {
		return nil, fmt.Errorf("write %s: %w", archive, err)
	}
	ok = true
	for _, p := range []string{cfg.Daemon.TLS.CertFile, cfg.Daemon.TLS.KeyFile} {
		if p != "" && !strings.HasPrefix(p, dataDir+string(filepath.Separator)) {
			fmt.Fprintf(warnOut, "note: %s is outside the data dir and is not in the backup\n", p)
		}
	}
	fi, _ := os.Stat(archive)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}
	fmt.Fprintf(out, "Wrote %s (%d bytes, mode 0600): config, database (schema %d), %d data files; master key: %s.\n",
		archive, size, m.SchemaVersion, files+1, describeKeyState(m.MasterKey))
	return m, nil
}

func describeKeyState(s string) string {
	switch s {
	case keyInArchive:
		return "included (master.key)"
	case keyExported:
		return "exported from the OS keychain into the archive"
	case keyNotIncluded:
		return "NOT included (it is in the OS keychain)"
	}
	return "none yet"
}

func newRestoreCmd(g *globals) *cobra.Command {
	var force bool
	var dataDir string
	cmd := &cobra.Command{
		Use:   "restore <file.tar.gz>",
		Short: "Restore a backup made with `bosun backup` (stop bosund first)",
		Long: `restore unpacks a backup into the data dir named by the archived config (or
--data-dir) and writes the config to --config. It refuses to run while the
daemon answers, refuses a data dir that is not empty unless --force (which
moves the existing data dir and config aside, never deletes them), and
refuses archives whose database is newer than this binary understands.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestore(cmd.Context(), cmd.OutOrStdout(), g, args[0], dataDir, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restore over a non-empty data dir (the old one is moved aside)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "restore into this data dir instead of the archived config's daemon.data_dir")
	return cmd
}

// safeEntryName validates a tar entry name and returns it cleaned.
func safeEntryName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return "", fmt.Errorf("unsafe path %q in archive", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe path %q in archive", name)
	}
	return clean, nil
}

func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func runRestore(ctx context.Context, out io.Writer, g *globals, archive, dataDirFlag string, force bool) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: not a gzip archive: %w", archive, err)
	}
	tr := tar.NewReader(gz)

	// The manifest comes first: check it before writing anything.
	hdr, err := tr.Next()
	if err != nil || hdr.Name != backupManifestName {
		return fmt.Errorf("%s: not a bosun backup (no %s)", archive, backupManifestName)
	}
	var m backupManifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return fmt.Errorf("%s: bad manifest: %w", archive, err)
	}
	if m.Format != backupFormat {
		return fmt.Errorf("%s: backup format %d is not supported by this bosun (want %d)", archive, m.Format, backupFormat)
	}
	latest, err := store.LatestSchemaVersion()
	if err != nil {
		return err
	}
	if m.SchemaVersion > latest {
		return fmt.Errorf("%s: its database is at schema version %d, but this bosun (%s) only knows up to %d: upgrade bosun first", archive, m.SchemaVersion, version, latest)
	}
	hdr, err = tr.Next()
	if err != nil || hdr.Name != backupConfigName {
		return fmt.Errorf("%s: missing %s", archive, backupConfigName)
	}
	cfgBytes, err := io.ReadAll(io.LimitReader(tr, 1<<20))
	if err != nil {
		return err
	}
	cfg, err := config.Parse(cfgBytes)
	if err != nil {
		return fmt.Errorf("archived config: %w", err)
	}
	oldDataDir := cfg.Daemon.DataDir
	dataDir := oldDataDir
	if dataDirFlag != "" {
		if dataDir, err = filepath.Abs(dataDirFlag); err != nil {
			return err
		}
	}
	if dataDir != oldDataDir {
		// Rewrite the config for the new location (TLS files under the old
		// data dir move with it).
		raw, err := config.Parse(cfgBytes)
		if err != nil {
			return err
		}
		raw.Daemon.DataDir = dataDir
		for _, p := range []*string{&raw.Daemon.TLS.CertFile, &raw.Daemon.TLS.KeyFile} {
			if rel, err := filepath.Rel(oldDataDir, *p); *p != "" && err == nil && !strings.HasPrefix(rel, "..") {
				*p = filepath.Join(dataDir, rel)
			}
		}
		cfg = raw
	}
	cfgPath, err := filepath.Abs(g.configPath)
	if err != nil {
		return err
	}

	// Refuse while a daemon answers at the restored config's address.
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, herr := daemonVersion(hctx, cfg.BaseURL())
	cancel()
	if herr == nil {
		return fmt.Errorf("bosund is running at %s: stop it first (`bosun service stop`)", cfg.BaseURL())
	}

	empty, err := dirEmpty(dataDir)
	if err != nil {
		return err
	}
	_, cfgStatErr := os.Stat(cfgPath)
	cfgInData := strings.HasPrefix(cfgPath, dataDir+string(filepath.Separator))
	stamp := time.Now().Format("20060102-150405")
	if !empty || (cfgStatErr == nil && !cfgInData) {
		if !force {
			what := dataDir + " is not empty"
			if empty {
				what = cfgPath + " exists"
			}
			return fmt.Errorf("%s: pass --force to restore anyway (the existing data dir and config are moved aside, not deleted)", what)
		}
		if !empty {
			aside := dataDir + ".before-restore-" + stamp
			if err := os.Rename(dataDir, aside); err != nil {
				return fmt.Errorf("move the existing data dir aside: %w", err)
			}
			fmt.Fprintf(out, "Moved the existing data dir to %s\n", aside)
		}
		if _, err := os.Stat(cfgPath); err == nil && !cfgInData {
			aside := cfgPath + ".before-restore-" + stamp
			if err := os.Rename(cfgPath, aside); err != nil {
				return fmt.Errorf("move the existing config aside: %w", err)
			}
			fmt.Fprintf(out, "Moved the existing config to %s\n", aside)
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return err
	}

	// Extract data/ entries.
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}
		name, err := safeEntryName(hdr.Name)
		if err != nil {
			return err
		}
		rel, ok := strings.CutPrefix(name, strings.TrimSuffix(backupDataPrefix, "/")+"/")
		if !ok {
			return fmt.Errorf("unexpected entry %q in archive", hdr.Name)
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > maxBackupEntry {
				return fmt.Errorf("entry %q is too large", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			wf, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(wf, io.LimitReader(tr, hdr.Size))
			if err := errors.Join(cerr, wf.Close()); err != nil {
				return fmt.Errorf("restore %s: %w", dst, err)
			}
			files++
		default:
			return fmt.Errorf("entry %q: only regular files and directories are allowed", hdr.Name)
		}
	}

	// Verify the restored database.
	st, err := store.Open(ctx, filepath.Join(dataDir, "bosun.db"))
	if err != nil {
		return fmt.Errorf("restored database: %w", err)
	}
	sv, err := st.SchemaVersion(ctx)
	var check string
	if err == nil {
		err = st.DB().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
	}
	st.Close()
	if err != nil {
		return fmt.Errorf("restored database: %w", err)
	}
	if check != "ok" || sv != m.SchemaVersion {
		return fmt.Errorf("restored database failed verification (quick_check %q, schema %d, manifest %d)", check, sv, m.SchemaVersion)
	}

	// The config.
	if dataDir != oldDataDir {
		if err := config.Save(cfgPath, cfg); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "Restored %d files into %s and the config to %s (schema %d, made by bosun %s on %s).\n",
		files, dataDir, cfgPath, m.SchemaVersion, m.BosunVersion, m.CreatedAt.Local().Format(time.DateTime))
	switch m.MasterKey {
	case keyNotIncluded:
		fmt.Fprintln(out, `
WARNING: this backup does not contain the vault master key (it was in the OS
keychain). Stored credentials can only be decrypted if this machine's keychain
holds the same bosun master key; otherwise re-enter them with
`+"`bosun credentials set`"+` / `+"`bosun connect`"+`.`)
	case keyExported:
		fmt.Fprintln(out, "The master key exported from the keychain was restored to master.key. On first start, bosund\n"+
			"imports it into this machine's keychain if that has no bosun key (or set daemon.master_key: file).")
	}
	fmt.Fprintln(out, "Next: start the daemon (`bosun service start` or `bosund`), log in again (`bosun login`), and run `bosun doctor`.")
	return nil
}
