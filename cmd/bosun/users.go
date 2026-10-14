package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/perishabledave/bosun/internal/api"
	"github.com/perishabledave/bosun/internal/auth"
)

func newUsersCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage household users (admin only)",
		Long: "Add household members, disable or remove them, and reset forgotten passwords.\n" +
			"New members get a one-time invite code (valid 24 hours) and choose their own password\n" +
			"with `bosun accept-invite`, so you never need to know it.",
	}
	cmd.AddCommand(newUsersListCmd(g), newUsersAddCmd(g),
		newUsersSetDisabledCmd(g, "disable", true), newUsersSetDisabledCmd(g, "enable", false),
		newUsersRoleCmd(g), newUsersRemoveCmd(g), newUsersResetCmd(g))
	return cmd
}

func newUsersListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.UserListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/users", tok, nil, &resp); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "USERNAME\tROLE\tSTATUS\tLAST LOGIN\tCREATED")
			for _, u := range resp.Users {
				status := "active"
				switch {
				case u.Disabled:
					status = "disabled"
				case u.InviteExpiresAt != nil:
					status = "invited (until " + u.InviteExpiresAt.Local().Format(time.DateTime) + ")"
				}
				last := "-"
				if u.LastLoginAt != nil {
					last = u.LastLoginAt.Local().Format(time.DateTime)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Username, u.Role, status, last, u.CreatedAt.Local().Format(time.DateOnly))
			}
			return tw.Flush()
		},
	}
}

// findUser resolves a username to its ID.
func findUser(ctx context.Context, base, tok, username string) (*auth.UserInfo, error) {
	var resp api.UserListResponse
	if err := call(ctx, "GET", base+"/v1/users", tok, nil, &resp); err != nil {
		return nil, err
	}
	for _, u := range resp.Users {
		if strings.EqualFold(u.Username, username) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("no user named %q (see `bosun users list`)", username)
}

func newUsersAddCmd(g *globals) *cobra.Command {
	var role string
	var pwStdin bool
	cmd := &cobra.Command{
		Use:   "add <username>",
		Short: "Add a user and print a one-time invite code for them",
		Long: "Add a household user. By default this prints a one-time invite code (valid 24 hours)\n" +
			"and the command the new member runs to choose their own password. With --password-stdin\n" +
			"the password is read from stdin instead and no invite is issued.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role = strings.ToLower(role)
			if role != "admin" && role != "member" {
				return fmt.Errorf("--role must be admin or member, got %q", role)
			}
			if err := auth.ValidateUsername(args[0]); err != nil {
				return errors.New(strings.TrimPrefix(err.Error(), auth.ErrInvalidInput.Error()+": "))
			}
			req := api.CreateUserRequest{Username: args[0], Role: role}
			if pwStdin {
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 2048))
				if err != nil {
					return fmt.Errorf("read password: %w", err)
				}
				req.Password = strings.TrimRight(string(b), "\r\n")
				if req.Password == "" {
					return errors.New("empty password on stdin")
				}
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.UserInviteResponse
			err = call(cmd.Context(), "POST", base+"/v1/users", tok, req, &resp)
			req.Password = ""
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if resp.Invite == nil {
				fmt.Fprintf(out, "Created %s %s. They can log in with `bosun login -u %s`.\n", resp.User.Role, resp.User.Username, resp.User.Username)
				return nil
			}
			fmt.Fprintf(out, "Created %s %s.\n", resp.User.Role, resp.User.Username)
			printInvite(out, g, base, resp.User.Username, resp.Invite)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "member", "role: admin or member")
	cmd.Flags().BoolVar(&pwStdin, "password-stdin", false, "read the user's password from stdin instead of issuing an invite")
	return cmd
}

// printInvite shows an invite code and how to redeem it.
func printInvite(out io.Writer, g *globals, base, username string, inv *auth.Invite) {
	fmt.Fprintf(out, "\nInvite code for %s (single use, expires %s):\n\n    %s\n\n", username, inv.ExpiresAt.Local().Format(time.DateTime), inv.Code)
	fmt.Fprintf(out, "Give it to %s privately. On their own computer they run:\n\n    %s\n\n", username, acceptInviteCommand(g, base, inv.Code))
	fmt.Fprintf(out, "or open %s in a browser and choose \"Have an invite code?\" on the sign-in page.\n", base)
}

// acceptInviteCommand is the command line a new member runs. It names the
// server unless the daemon only listens on loopback, and the certificate
// fingerprint when the CLI pins one.
func acceptInviteCommand(g *globals, base, code string) string {
	parts := []string{"bosun", "accept-invite", code}
	if !isLoopbackURL(base) {
		parts = append(parts, "--server", base)
		fp := g.fingerprint
		if s := g.loadSavedServer(); fp == "" && g.caFile == "" && origin(s.Server) == origin(base) {
			fp = s.Fingerprint
		}
		if fp != "" && strings.HasPrefix(base, "https:") {
			parts = append(parts, "--insecure-fingerprint", fp)
		}
	}
	return strings.Join(parts, " ")
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func updateUser(cmd *cobra.Command, g *globals, username string, req api.UpdateUserRequest) (*auth.UserInfo, error) {
	base, tok, err := g.authed()
	if err != nil {
		return nil, err
	}
	u, err := findUser(cmd.Context(), base, tok, username)
	if err != nil {
		return nil, err
	}
	var info auth.UserInfo
	if err := call(cmd.Context(), "PUT", fmt.Sprintf("%s/v1/users/%d", base, u.ID), tok, req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func newUsersSetDisabledCmd(g *globals, verb string, disabled bool) *cobra.Command {
	short := "Re-enable a disabled user"
	if disabled {
		short = "Disable a user (signs them out everywhere; their data is kept)"
	}
	return &cobra.Command{
		Use:   verb + " <username>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := updateUser(cmd, g, args[0], api.UpdateUserRequest{Disabled: &disabled})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s.\n", info.Username, map[bool]string{true: "disabled", false: "enabled"}[info.Disabled])
			return nil
		},
	}
}

func newUsersRoleCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "set-role <username> admin|member",
		Short: "Change a user's role",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			role := strings.ToLower(args[1])
			if role != "admin" && role != "member" {
				return fmt.Errorf("role must be admin or member, got %q", args[1])
			}
			info, err := updateUser(cmd, g, args[0], api.UpdateUserRequest{Role: &role})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s.\n", info.Username, info.Role)
			return nil
		},
	}
}

func newUsersRemoveCmd(g *globals) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <username>",
		Short: "Delete a user and all their data (conversations, memories, credentials, routines, ...)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			u, err := findUser(cmd.Context(), base, tok, args[0])
			if err != nil {
				return err
			}
			if !yes {
				p := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
				fmt.Fprintf(cmd.OutOrStdout(), "This permanently deletes %s and all their conversations, memories, credentials,\n"+
					"routines, policy rules and chat links. The audit log is kept.\n", u.Username)
				ans, err := p.line(fmt.Sprintf("Type the username (%s) to confirm", u.Username), "")
				if err != nil {
					return err
				}
				if !strings.EqualFold(ans, u.Username) {
					return errors.New("not confirmed; nothing deleted")
				}
			}
			if err := call(cmd.Context(), "DELETE", fmt.Sprintf("%s/v1/users/%d", base, u.ID), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s.\n", u.Username)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func newUsersResetCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "reset-password <username>",
		Short: "Sign a user out and issue an invite code with which they choose a new password",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			u, err := findUser(cmd.Context(), base, tok, args[0])
			if err != nil {
				return err
			}
			var resp api.UserInviteResponse
			if err := call(cmd.Context(), "POST", fmt.Sprintf("%s/v1/users/%d/reset-password", base, u.ID), tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s's password was reset and their sessions were signed out.\n", resp.User.Username)
			printInvite(out, g, base, resp.User.Username, resp.Invite)
			return nil
		},
	}
}

// newPasswordTwice prompts for a new password and its confirmation.
func newPasswordTwice(p *prompter, label string) (string, error) {
	for range 3 {
		pw, err := p.password(label)
		if err != nil {
			return "", err
		}
		if err := auth.ValidatePassword(pw); err != nil {
			fmt.Fprintln(p.out, strings.TrimPrefix(err.Error(), auth.ErrInvalidInput.Error()+": "))
			continue
		}
		again, err := p.password("Repeat it")
		if err != nil {
			return "", err
		}
		if again != pw {
			fmt.Fprintln(p.out, "The passwords do not match.")
			continue
		}
		return pw, nil
	}
	return "", errors.New("no password set")
}

func newAcceptInviteCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "accept-invite <code>",
		Short: "Redeem an invite code: choose your password and log in",
		Long: "Redeem the one-time invite code an admin gave you. You choose your password (typed twice);\n" +
			"the CLI is then logged in as you. Pass --server (and --insecure-fingerprint for a\n" +
			"self-signed daemon certificate) as shown by the admin.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := g.baseURL()
			if err != nil {
				return err
			}
			code := auth.NormalizeInviteCode(args[0])
			p := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
			pw, err := newPasswordTwice(p, "Choose a password")
			if err != nil {
				return err
			}
			var lr api.LoginResponse
			err = call(cmd.Context(), "POST", base+"/v1/invites/"+url.PathEscape(code)+"/accept", "",
				api.AcceptInviteRequest{Password: pw, Label: "cli"}, &lr)
			pw = ""
			if err != nil {
				return err
			}
			path, err := g.writeToken(lr.Token)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Welcome, %s. You are logged in; token saved to %s\n", lr.User.Username, path)
			if err := g.rememberServer(cmd.OutOrStdout()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Try `bosun chat`, or `bosun connect email` to link your mail and calendar.")
			return nil
		},
	}
}

func newPasswdCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "passwd",
		Short: "Change your password (signs out your other sessions)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			p := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
			cur, err := p.password("Current password")
			if err != nil {
				return err
			}
			pw, err := newPasswordTwice(p, "New password")
			if err != nil {
				return err
			}
			err = call(cmd.Context(), "PUT", base+"/v1/me/password", tok, api.ChangePasswordRequest{Current: cur, New: pw}, nil)
			cur, pw = "", ""
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Password changed. Your other sessions were signed out; this one stays logged in.")
			return nil
		},
	}
}
