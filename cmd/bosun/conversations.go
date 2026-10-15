package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/api"
)

func convURL(base, id string) string { return base + "/v1/conversations/" + url.PathEscape(id) }

func newConversationsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "conversations",
		Aliases: []string{"conv"},
		Short:   "List, rename, pin, archive, export, or delete your conversations",
	}

	var archived, query string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List your conversations (pinned first, then most recent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			q := url.Values{}
			q.Set("archived", archived)
			q.Set("limit", strconv.Itoa(limit))
			if query != "" {
				q.Set("q", query)
			}
			var resp api.ConversationListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/conversations?"+q.Encode(), tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Conversations) == 0 {
				fmt.Fprintln(out, "No conversations.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTITLE\tFLAGS\tMESSAGES\tLAST ACTIVITY")
			for _, c := range resp.Conversations {
				title := c.Title
				if title == "" {
					title = "(untitled)"
				}
				var flags []string
				if c.Pinned {
					flags = append(flags, "pinned")
				}
				if c.Archived {
					flags = append(flags, "archived")
				}
				if c.Unread {
					flags = append(flags, "unread")
				}
				if c.Source != "chat" {
					flags = append(flags, c.Source)
				}
				last := c.CreatedAt
				if c.LastMessageAt != nil {
					last = *c.LastMessageAt
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", c.ID, oneLineTrunc(title, 50), strings.Join(flags, ","),
					c.MessageCount, last.Local().Format("2006-01-02 15:04"))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if resp.NextCursor != nil {
				fmt.Fprintf(out, "(more: raise --limit, up to %d)\n", api.MaxConversationPage)
			}
			return nil
		},
	}
	list.Flags().StringVar(&archived, "archived", "false", "false (default), true, or all")
	list.Flags().StringVarP(&query, "search", "q", "", "only conversations whose title or messages contain this text")
	list.Flags().IntVar(&limit, "limit", api.DefaultConversationPage, "how many to show")

	patch := func(use, short string, nargs int, body func(args []string) api.PatchConversationRequest, done func(c api.ConversationResponse) string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.MinimumNArgs(nargs),
			RunE: func(cmd *cobra.Command, args []string) error {
				base, tok, err := g.authed()
				if err != nil {
					return err
				}
				var c api.ConversationResponse
				if err := call(cmd.Context(), "PATCH", convURL(base, args[0]), tok, body(args), &c); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), done(c))
				return nil
			},
		}
	}
	yes, no := true, false
	rename := patch("rename <id> <title...>", "Rename a conversation", 2,
		func(args []string) api.PatchConversationRequest {
			t := strings.Join(args[1:], " ")
			return api.PatchConversationRequest{Title: &t}
		},
		func(c api.ConversationResponse) string { return fmt.Sprintf("%s: renamed to %q", c.ID, c.Title) })
	pin := patch("pin <id>", "Pin a conversation to the top of the list", 1,
		func([]string) api.PatchConversationRequest { return api.PatchConversationRequest{Pinned: &yes} },
		func(c api.ConversationResponse) string { return c.ID + ": pinned" })
	unpin := patch("unpin <id>", "Unpin a conversation", 1,
		func([]string) api.PatchConversationRequest { return api.PatchConversationRequest{Pinned: &no} },
		func(c api.ConversationResponse) string { return c.ID + ": unpinned" })
	archive := patch("archive <id>", "Archive a conversation (hidden from the default list)", 1,
		func([]string) api.PatchConversationRequest { return api.PatchConversationRequest{Archived: &yes} },
		func(c api.ConversationResponse) string { return c.ID + ": archived" })
	unarchive := patch("unarchive <id>", "Restore an archived conversation", 1,
		func([]string) api.PatchConversationRequest { return api.PatchConversationRequest{Archived: &no} },
		func(c api.ConversationResponse) string { return c.ID + ": unarchived" })
	for _, c := range []*cobra.Command{pin, unpin, archive, unarchive} {
		c.Args = cobra.ExactArgs(1)
	}

	del := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a conversation and its messages (pending approvals in it are canceled)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", convURL(base, args[0]), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), args[0]+": deleted")
			return nil
		},
	}

	var format, outPath string
	export := &cobra.Command{
		Use:   "export <id>",
		Short: "Export a conversation as Markdown (default) or JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "markdown" && format != "json" {
				return fmt.Errorf("--format must be markdown or json, got %q", format)
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			b, err := fetchBytes(cmd.Context(), convURL(base, args[0])+"/export?format="+format, tok)
			if err != nil {
				return err
			}
			if outPath == "" || outPath == "-" {
				_, err := cmd.OutOrStdout().Write(b)
				return err
			}
			if err := os.WriteFile(outPath, b, 0o600); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), outPath)
			return nil
		},
	}
	export.Flags().StringVar(&format, "format", "markdown", "markdown or json")
	export.Flags().StringVarP(&outPath, "output", "o", "", "file to write (default: standard output)")

	cmd.AddCommand(list, rename, pin, unpin, archive, unarchive, del, export)
	return cmd
}
