package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/api"
)

// fetchBytes GETs url with the token and returns the body (at most 16 MB).
func fetchBytes(ctx context.Context, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doHTTP(httpClient, req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w (is bosund running?)", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, apiError(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func newBrowserCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "Show or stop your assistant's web browser",
		Long: "The browser integration runs a private headless Chromium for you. `bosun browser` shows what it\n" +
			"is doing; `bosun browser stop` kills it (ending any task); `bosun browser screenshot` saves the\n" +
			"current view.",
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show your browser's current page and step count",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var st api.BrowserStatus
			if err := call(cmd.Context(), "GET", base+"/v1/browser", tok, nil, &st); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !st.Running {
				fmt.Fprintln(out, "Browser: not running.")
			} else {
				fmt.Fprintf(out, "Browser: running\nPage:    %s\nURL:     %s\n", st.Title, st.URL)
			}
			fmt.Fprintf(out, "Steps:   %d of %d in the latest task\n", st.Steps, st.Budget)
			return nil
		},
	}
	cmd.AddCommand(status)
	cmd.RunE = status.RunE
	cmd.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Kill your browser (ends the current task; your sign-ins in its profile are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "POST", base+"/v1/browser/stop", tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Browser stopped.")
			return nil
		},
	})
	var outPath string
	shot := &cobra.Command{
		Use:   "screenshot",
		Short: "Save a screenshot of your browser's current page",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			b, err := fetchBytes(cmd.Context(), base+"/v1/browser/screenshot", tok)
			if err != nil {
				return err
			}
			p := outPath
			if p == "" {
				p = filepath.Join(os.TempDir(), "bosun-browser.png")
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
	shot.Flags().StringVarP(&outPath, "output", "o", "", "file to write (default: bosun-browser.png in the temp directory)")
	cmd.AddCommand(shot)
	return cmd
}
