package main

import (
	"fmt"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
)

func newChannelsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "channels",
		Aliases: []string{"channel"},
		Short:   "Talk to your assistant from chat apps (Telegram)",
		Long: "Channels let you text your assistant from a chat app. A chat acts as you only after you link it:\n" +
			"`bosun channels link telegram` prints a one-time code (valid 10 minutes) that you send to the bot.",
	}

	link := &cobra.Command{
		Use:     "link <channel>",
		Short:   "Get a one-time code to link a chat to your account",
		Example: "  bosun channels link telegram",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var lc api.ChannelLinkCodeResponse
			if err := call(cmd.Context(), "POST", base+"/v1/channels/"+url.PathEscape(args[0])+"/link", tok, nil, &lc); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Link code: %s\n", lc.Code)
			fmt.Fprintln(out, lc.Instructions)
			fmt.Fprintf(out, "The code expires at %s and works once.\n", lc.ExpiresAt.Local().Format(time.Kitchen))
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your linked chats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.ChannelListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/channels", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			enabled := "none"
			if len(resp.Channels) > 0 {
				enabled = strings.Join(resp.Channels, ", ")
			}
			fmt.Fprintf(out, "Enabled channels: %s\n", enabled)
			if len(resp.Links) == 0 {
				fmt.Fprintln(out, "No linked chats. Link one with `bosun channels link <channel>`.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CHANNEL\tID\tNAME\tLINKED")
			for _, l := range resp.Links {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", l.Channel, l.ExternalID, l.DisplayName, l.CreatedAt.Local().Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}

	unlink := &cobra.Command{
		Use:     "unlink <channel> <id>",
		Aliases: []string{"rm"},
		Short:   "Unlink a chat (see `bosun channels list` for ids)",
		Example: "  bosun channels unlink telegram 123456789",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", base+"/v1/channels/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unlinked %s chat %s.\n", args[0], args[1])
			return nil
		},
	}

	cmd.AddCommand(link, list, unlink)
	return cmd
}
