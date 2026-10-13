package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
	"github.com/dperedo/bosun/internal/memory"
)

func validKind(k string) (string, error) {
	k = strings.ToLower(k)
	if !memory.Kind(k).Valid() {
		return "", fmt.Errorf("kind must be fact, preference, summary, or note, got %q", k)
	}
	return k, nil
}

func parseMemoryID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid memory id %q (see `bosun memory list`)", s)
	}
	return id, nil
}

func newMemoryCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Show and edit what the assistant remembers about you",
		Long: "Memories are facts, preferences, summaries, and notes the assistant recalls in later\n" +
			"conversations. Some are extracted automatically after each turn; you can add, edit, and\n" +
			"forget them here. Only you can see your memories.",
	}

	var kind, search string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List your memories (newest first), or search them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if kind != "" {
				k, err := validKind(kind)
				if err != nil {
					return err
				}
				q.Set("kind", k)
			}
			if search != "" {
				q.Set("q", search)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			u := base + "/v1/memory"
			if len(q) > 0 {
				u += "?" + q.Encode()
			}
			var resp api.MemoryListResponse
			if err := call(cmd.Context(), "GET", u, tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Memories) == 0 {
				fmt.Fprintln(out, "No memories.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tKIND\tUPDATED\tSOURCE\tCONTENT")
			for _, m := range resp.Memories {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", m.ID, m.Kind, m.UpdatedAt.Local().Format("2006-01-02"),
					m.Source, oneLineTrunc(m.Content, 80))
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&kind, "kind", "", "only this kind: fact, preference, summary, or note")
	list.Flags().StringVar(&search, "search", "", "full-text search (all words must match)")
	list.Flags().IntVar(&limit, "limit", 0, "maximum number of memories (server default 50, max 500)")
	cmd.AddCommand(list)

	cmd.AddCommand(&cobra.Command{
		Use:   "add <kind> <content>",
		Short: "Add a memory (kind: fact, preference, summary, or note)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := validKind(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var m api.MemoryResponse
			if err := call(cmd.Context(), "POST", base+"/v1/memory", tok,
				api.CreateMemoryRequest{Kind: k, Content: strings.Join(args[1:], " ")}, &m); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Remembered %d (%s): %s\n", m.ID, m.Kind, m.Content)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "edit <id> <content>",
		Short: "Replace the content of a memory",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseMemoryID(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var m api.MemoryResponse
			if err := call(cmd.Context(), "PUT", fmt.Sprintf("%s/v1/memory/%d", base, id), tok,
				api.UpdateMemoryRequest{Content: strings.Join(args[1:], " ")}, &m); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %d (%s): %s\n", m.ID, m.Kind, m.Content)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "forget <id>",
		Short: "Delete a memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseMemoryID(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", fmt.Sprintf("%s/v1/memory/%d", base, id), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forgot %d\n", id)
			return nil
		},
	})
	return cmd
}
