package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/dperedo/bosun/internal/approval"
	"github.com/dperedo/bosun/internal/audit"
	"github.com/dperedo/bosun/internal/config"
	"github.com/dperedo/bosun/internal/hooks"
	"github.com/dperedo/bosun/internal/hooks/builtin"
	"github.com/dperedo/bosun/internal/hooks/script"
)

// buildHooks returns the hook registry: the built-in hooks enabled in config,
// then (only if hooks.script_hooks.enabled) the configured script hooks, each
// logged at WARN level since they run admin-supplied executables.
func buildHooks(cfg *config.Config, aud audit.Logger, log *slog.Logger) (*hooks.Registry, error) {
	reg := hooks.NewRegistry()
	reg.SetLogger(log)
	if cfg.Hooks.RedactSecretsEnabled() {
		if err := reg.AddPreModelCall(builtin.RedactSecretsName, builtin.RedactSecrets); err != nil {
			return nil, err
		}
	}
	sh := cfg.Hooks.ScriptHooks
	if !sh.Enabled {
		if len(sh.Scripts) > 0 {
			log.Info("script hooks are configured but hooks.script_hooks.enabled is false; not running them", "count", len(sh.Scripts))
		}
		return reg, nil
	}
	specs := make([]script.Spec, 0, len(sh.Scripts))
	for _, s := range sh.Scripts {
		specs = append(specs, script.Spec{Name: s.Name, Point: hooks.Point(s.Point), Command: s.Command, Timeout: s.Timeout, Match: s.Match})
	}
	hs, err := script.Register(reg, specs, script.Options{Audit: aud, Log: log})
	if err != nil {
		return nil, fmt.Errorf("hooks.script_hooks: %w", err)
	}
	for _, h := range hs {
		s := h.Spec()
		log.Warn("script hook enabled: runs an admin-supplied executable",
			"name", h.Name(), "point", s.Point, "command", strings.Join(s.Command, " "), "timeout", s.Timeout, "match", s.Match)
	}
	return reg, nil
}

// onApprovalHooks adapts the registry's on_approval hooks to
// approval.Options.OnResolve. Hooks run in the background (detached from the
// resolving request) so a slow notifier never delays an approval.
func onApprovalHooks(reg *hooks.Registry) func(context.Context, approval.Approval, approval.Decision) {
	return func(ctx context.Context, a approval.Approval, d approval.Decision) {
		if len(reg.OnApprovals()) == 0 {
			return
		}
		go reg.RunOnApproval(context.WithoutCancel(ctx), a, d)
	}
}
