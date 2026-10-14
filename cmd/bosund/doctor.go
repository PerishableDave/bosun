package main

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/perishabledave/bosun/internal/doctor"
	"github.com/perishabledave/bosun/internal/integration"
	"github.com/perishabledave/bosun/internal/llm/providers"
)

// doctorHTTP is used for the LLM reachability probes.
var doctorHTTP = &http.Client{Timeout: 8 * time.Second}

// doctorChecks runs the daemon-side checks for GET /v1/doctor: data dir,
// master key, each LLM provider (key source and reachability), every
// listed integration and channel, TLS, routines, disk space, and the
// reverse-proxy settings. Order is stable.
func (d *daemon) doctorChecks(ctx context.Context) []doctor.Check {
	cfg := d.cfg
	checks := []doctor.Check{
		doctor.DataDir(cfg.Daemon.DataDir),
		{Name: "master key", Status: doctor.OK, Detail: d.keyDesc + " (loaded)"},
	}
	checks = append(checks, d.llmChecks(ctx)...)

	listed := append([]integration.Integration(nil), d.listed...)
	sort.Slice(listed, func(i, j int) bool { return listed[i].Manifest().Name < listed[j].Manifest().Name })
	hasChannels := false
	for _, in := range listed {
		m := in.Manifest()
		if m.Name == providers.CredentialIntegration {
			continue // covered by the llm checks
		}
		state, msg := integration.StatusOK, ""
		if sr, ok := in.(integration.StatusReporter); ok {
			st := sr.Status()
			state, msg = st.State, st.Message
		}
		if m.Name == "channels" {
			hasChannels = true
		}
		checks = append(checks, doctor.Integration(m.Name, state, msg, len(in.Tools())))
	}
	if !hasChannels {
		checks = append(checks, doctor.Check{Name: "channels", Status: doctor.OK, Detail: "none enabled"})
	}

	checks = append(checks, doctor.TLS(cfg, time.Now()))
	if d.store != nil {
		total, enabled, err := d.store.CountRoutines(ctx)
		if err != nil {
			checks = append(checks, doctor.Check{Name: "routines", Status: doctor.Fail, Detail: err.Error()})
		} else {
			checks = append(checks, doctor.Routines(d.sched != nil && d.sched.Running(), total, enabled))
		}
	}
	checks = append(checks, doctor.Disk(cfg.Daemon.DataDir))
	var ignored int64
	var lastIP string
	if d.apiSrv != nil {
		ignored, lastIP = d.apiSrv.ProxyIgnored()
	}
	checks = append(checks, doctor.Proxy(cfg, ignored, lastIP))
	return checks
}

// llmChecks probes every configured provider concurrently.
func (d *daemon) llmChecks(ctx context.Context) []doctor.Check {
	cfg := d.cfg
	names := make([]string, 0, len(cfg.LLM.Providers))
	for n := range cfg.LLM.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	required := doctor.RequiredProviders(cfg)
	out := make([]doctor.Check, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pc := cfg.LLM.Providers[name]
			key, src, err := d.llmKeys.Resolve(ctx, name)
			if err != nil {
				st := doctor.Warn
				if required[name] {
					st = doctor.Fail
				}
				out[i] = doctor.Check{Name: "llm " + name, Status: st, Detail: "reading the key from the vault failed: " + err.Error()}
				return
			}
			d.log.Info("doctor: probing llm provider", "provider", name)
			out[i] = doctor.LLM(ctx, doctorHTTP, name, pc, key, doctor.KeySource{Source: src, Env: pc.APIKeyEnv}, required[name], true)
		}()
	}
	wg.Wait()
	return out
}
