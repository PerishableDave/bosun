package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/auth"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// call performs a JSON request; on non-2xx it returns the API error message.
func call(ctx context.Context, method, url, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w (is bosund running?)", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var eb api.ErrorBody
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&eb) == nil && eb.Error.Message != "" {
			return fmt.Errorf("%s (HTTP %d)", eb.Error.Message, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, url, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check that the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := g.baseURL()
			if err != nil {
				return err
			}
			var h api.HealthResponse
			if err := call(cmd.Context(), "GET", base+"/v1/health", "", nil, &h); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "bosund %s at %s (version %s)\n", h.Status, base, h.Version)
			if tok, err := g.readToken(); err == nil {
				var me auth.User
				if err := call(cmd.Context(), "GET", base+"/v1/me", tok, nil, &me); err == nil {
					fmt.Fprintf(out, "logged in as %s (%s)\n", me.Username, me.Role)
				} else {
					fmt.Fprintf(out, "stored token is not valid: %v\n", err)
				}
			} else {
				fmt.Fprintln(out, "not logged in (run `bosun login`)")
			}
			return nil
		},
	}
}

func newLoginCmd(g *globals) *cobra.Command {
	var username string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in and store a session token for the CLI",
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := g.baseURL()
			if err != nil {
				return err
			}
			p := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
			if username == "" {
				def := os.Getenv("USER")
				if auth.ValidateUsername(def) != nil {
					def = ""
				}
				if username, err = p.line("Username", def); err != nil {
					return err
				}
			}
			pw, err := p.password("Password")
			if err != nil {
				return err
			}
			var lr api.LoginResponse
			req := api.LoginRequest{Username: username, Password: pw, Label: "cli"}
			if err := call(cmd.Context(), "POST", base+"/v1/login", "", req, &lr); err != nil {
				return err
			}
			path, err := g.writeToken(lr.Token)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s; token saved to %s\n", lr.User.Username, path)
			return nil
		},
	}
	cmd.Flags().StringVarP(&username, "username", "u", "", "username (prompted if empty)")
	return cmd
}

// tokenPath returns $data_dir/cli-token. (OS keychain storage comes later.)
func (g *globals) tokenPath() (string, error) {
	cfg, err := g.loadConfig()
	if err != nil {
		return "", err
	}
	return cfg.CLITokenPath(), nil
}

func (g *globals) readToken() (string, error) {
	path, err := g.tokenPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// writeToken stores the token with mode 0600, replacing any previous one.
func (g *globals) writeToken(token string) (string, error) {
	path, err := g.tokenPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("write token: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	_, werr := f.WriteString(token + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("write token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	return path, nil
}
