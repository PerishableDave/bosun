package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dperedo/bosun/internal/api"
)

func parseRoutineID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid routine id %q (see `bosun routines list`)", s)
	}
	return id, nil
}

func triggerString(t api.RoutineTrigger) string {
	if t.Cron != "" {
		return "cron " + t.Cron
	}
	return "event " + t.Event
}

func whenString(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// routineFlags are the flags shared by add and edit.
type routineFlags struct {
	name, prompt, cron, event, model string
	deliver                          []string
}

func (f *routineFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.name, "name", "", "routine name (unique per user)")
	cmd.Flags().StringVar(&f.prompt, "prompt", "", "what the assistant should do on each run")
	cmd.Flags().StringVar(&f.cron, "cron", "", `cron schedule, e.g. "0 7 * * 1-5" or @daily (daemon time zone)`)
	cmd.Flags().StringVar(&f.event, "event", "", "event name that triggers the routine, e.g. email.received")
	cmd.Flags().StringVar(&f.model, "model", "", `model override as <provider>/<model> ("" = default)`)
	cmd.Flags().StringSliceVar(&f.deliver, "deliver", nil, "where results are posted: web,cli (default both)")
	cmd.MarkFlagsMutuallyExclusive("cron", "event")
}

func printRoutine(w io.Writer, r api.RoutineResponse) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	model := r.Model
	if model == "" {
		model = "(default)"
	}
	fmt.Fprintf(tw, "ID:\t%d\n", r.ID)
	fmt.Fprintf(tw, "Name:\t%s\n", r.Name)
	fmt.Fprintf(tw, "Trigger:\t%s\n", triggerString(r.Trigger))
	fmt.Fprintf(tw, "Enabled:\t%v\n", r.Enabled)
	fmt.Fprintf(tw, "Model:\t%s\n", model)
	fmt.Fprintf(tw, "Deliver:\t%s\n", strings.Join(r.Deliver, ","))
	fmt.Fprintf(tw, "Next run:\t%s\n", whenString(r.NextRunAt))
	fmt.Fprintf(tw, "Last run:\t%s\n", whenString(r.LastRunAt))
	fmt.Fprintf(tw, "Prompt:\t%s\n", r.Prompt)
	tw.Flush()
}

func printRuns(w io.Writer, runs []api.RoutineRunResponse) error {
	if len(runs) == 0 {
		fmt.Fprintln(w, "No runs yet.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTARTED\tSOURCE\tSTATUS\tCONVERSATION\tRESULT")
	for _, r := range runs {
		result := r.Reply
		if r.Error != "" {
			result = r.Error
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"),
			r.Source, r.Status, r.ConversationID, oneLineTrunc(result, 60))
	}
	return tw.Flush()
}

func newRoutinesCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "routines",
		Aliases: []string{"routine"},
		Short:   "Manage routines: prompts the assistant runs on a schedule or when an event happens",
		Long: "A routine runs a prompt as you, on a cron schedule or when an event happens, with the same\n" +
			"permissions as your interactive chats: tool calls that need approval wait for you.\n" +
			"Each run gets its own conversation titled \"routine: <name>\".",
	}

	var add routineFlags
	var disabled bool
	addCmd := &cobra.Command{
		Use:   "add",
		Short: "Create a routine",
		Example: `  bosun routines add --name morning-brief --cron "0 7 * * 1-5" --prompt "Summarize today's calendar."
  bosun routines add --name mail-triage --event email.received --prompt "Flag anything urgent."`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if add.name == "" || add.prompt == "" {
				return fmt.Errorf("--name and --prompt are required")
			}
			if add.cron == "" && add.event == "" {
				return fmt.Errorf("one of --cron or --event is required")
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			enabled := !disabled
			req := api.CreateRoutineRequest{Name: add.name, Prompt: add.prompt, Model: add.model, Deliver: add.deliver,
				Trigger: api.RoutineTrigger{Cron: add.cron, Event: add.event}, Enabled: &enabled}
			var r api.RoutineResponse
			if err := call(cmd.Context(), "POST", base+"/v1/routines", tok, req, &r); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created routine %d (%s, %s); next run %s\n", r.ID, r.Name, triggerString(r.Trigger), whenString(r.NextRunAt))
			return nil
		},
	}
	add.register(addCmd)
	addCmd.Flags().BoolVar(&disabled, "disabled", false, "create the routine disabled")

	list := &cobra.Command{
		Use:   "list",
		Short: "List your routines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.RoutineListResponse
			if err := call(cmd.Context(), "GET", base+"/v1/routines", tok, nil, &resp); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(resp.Routines) == 0 {
				fmt.Fprintln(out, "No routines. Create one with `bosun routines add`.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tTRIGGER\tENABLED\tNEXT RUN\tLAST RUN")
			for _, r := range resp.Routines {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%v\t%s\t%s\n", r.ID, r.Name, triggerString(r.Trigger), r.Enabled,
					whenString(r.NextRunAt), whenString(r.LastRunAt))
			}
			return tw.Flush()
		},
	}

	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a routine and its recent runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRoutineID(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var r api.RoutineResponse
			if err := call(cmd.Context(), "GET", fmt.Sprintf("%s/v1/routines/%d", base, id), tok, nil, &r); err != nil {
				return err
			}
			var runs api.RoutineRunListResponse
			if err := call(cmd.Context(), "GET", fmt.Sprintf("%s/v1/routines/%d/runs?limit=5", base, id), tok, nil, &runs); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printRoutine(out, r)
			fmt.Fprintln(out)
			return printRuns(out, runs.Runs)
		},
	}

	var edit routineFlags
	var enable, disable bool
	editCmd := &cobra.Command{
		Use:   "edit <id>",
		Short: "Change a routine (only the flags you pass are changed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRoutineID(args[0])
			if err != nil {
				return err
			}
			var req api.UpdateRoutineRequest
			f := cmd.Flags()
			if f.Changed("name") {
				req.Name = &edit.name
			}
			if f.Changed("prompt") {
				req.Prompt = &edit.prompt
			}
			if f.Changed("cron") {
				req.Trigger = &api.RoutineTrigger{Cron: edit.cron}
			}
			if f.Changed("event") {
				req.Trigger = &api.RoutineTrigger{Event: edit.event}
			}
			if f.Changed("model") {
				req.Model = &edit.model
			}
			if f.Changed("deliver") {
				req.Deliver = edit.deliver
			}
			if enable || disable {
				v := enable
				req.Enabled = &v
			}
			if req.Name == nil && req.Prompt == nil && req.Trigger == nil && req.Model == nil && req.Deliver == nil && req.Enabled == nil {
				return fmt.Errorf("nothing to change; pass at least one flag (see --help)")
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var r api.RoutineResponse
			if err := call(cmd.Context(), "PUT", fmt.Sprintf("%s/v1/routines/%d", base, id), tok, req, &r); err != nil {
				return err
			}
			printRoutine(cmd.OutOrStdout(), r)
			return nil
		},
	}
	edit.register(editCmd)
	editCmd.Flags().BoolVar(&enable, "enable", false, "enable the routine")
	editCmd.Flags().BoolVar(&disable, "disable", false, "disable the routine")
	editCmd.MarkFlagsMutuallyExclusive("enable", "disable")

	remove := &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Delete a routine and its run history (its conversations are kept)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRoutineID(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			if err := call(cmd.Context(), "DELETE", fmt.Sprintf("%s/v1/routines/%d", base, id), tok, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Routine %d deleted.\n", id)
			return nil
		},
	}

	var wait bool
	var waitTimeout time.Duration
	run := &cobra.Command{
		Use:   "run <id>",
		Short: "Run a routine now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRoutineID(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var r api.RoutineRunResponse
			if err := call(cmd.Context(), "POST", fmt.Sprintf("%s/v1/routines/%d/run", base, id), tok, nil, &r); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Started run %d of routine %d.\n", r.ID, id)
			if !wait {
				fmt.Fprintf(out, "See `bosun routines runs %d` for the result.\n", id)
				return nil
			}
			final, err := waitForRun(cmd.Context(), base, tok, id, r.ID, waitTimeout, out)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Status: %s\n", final.Status)
			if final.Error != "" {
				fmt.Fprintf(out, "Error: %s\n", final.Error)
			}
			if final.Reply != "" {
				fmt.Fprintln(out, final.Reply)
			}
			if final.Status != "done" {
				return fmt.Errorf("run %d ended with status %s", final.ID, final.Status)
			}
			return nil
		},
	}
	run.Flags().BoolVar(&wait, "wait", false, "wait for the run to finish and print its reply")
	run.Flags().DurationVar(&waitTimeout, "timeout", 20*time.Minute, "how long --wait waits")

	var limit int
	runs := &cobra.Command{
		Use:   "runs <id>",
		Short: "Show a routine's recent runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRoutineID(args[0])
			if err != nil {
				return err
			}
			base, tok, err := g.authed()
			if err != nil {
				return err
			}
			var resp api.RoutineRunListResponse
			if err := call(cmd.Context(), "GET", fmt.Sprintf("%s/v1/routines/%d/runs?limit=%d", base, id, limit), tok, nil, &resp); err != nil {
				return err
			}
			return printRuns(cmd.OutOrStdout(), resp.Runs)
		},
	}
	runs.Flags().IntVar(&limit, "limit", 20, "number of runs to show (1-200)")

	cmd.AddCommand(list, addCmd, show, editCmd, remove, run, runs)
	return cmd
}

// runPollInterval is how often `routines run --wait` polls (tests shorten it).
var runPollInterval = time.Second

// waitForRun polls the routine's runs until runID finishes.
func waitForRun(ctx context.Context, base, tok string, routineID, runID int64, timeout time.Duration, out io.Writer) (api.RoutineRunResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	notified := false
	for {
		var resp api.RoutineRunListResponse
		if err := call(ctx, "GET", fmt.Sprintf("%s/v1/routines/%d/runs?limit=50", base, routineID), tok, nil, &resp); err != nil {
			return api.RoutineRunResponse{}, err
		}
		for _, r := range resp.Runs {
			if r.ID != runID {
				continue
			}
			switch r.Status {
			case "done", "error", "canceled":
				return r, nil
			case "waiting_approval":
				if !notified {
					fmt.Fprintln(out, "Waiting for approval (see `bosun approvals`)...")
					notified = true
				}
			}
		}
		select {
		case <-ctx.Done():
			return api.RoutineRunResponse{}, fmt.Errorf("gave up waiting for run %d: %w", runID, ctx.Err())
		case <-time.After(runPollInterval):
		}
	}
}
