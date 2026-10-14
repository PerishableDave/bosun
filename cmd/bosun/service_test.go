package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRunner records service-manager commands; loaded controls whether
// `launchctl print` succeeds.
type fakeRunner struct {
	calls  []string
	loaded bool
	show   string
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch {
	case name == "launchctl" && args[0] == "print":
		if !f.loaded {
			return nil, errors.New("not loaded")
		}
		return []byte("gui/501/x = {\n\tstate = running\n\tpid = 4242\n}\n"), nil
	case name == "launchctl" && args[0] == "bootstrap":
		f.loaded = true
	case name == "launchctl" && args[0] == "bootout":
		f.loaded = false
	case name == "systemctl" && strings.Contains(strings.Join(args, " "), "show"):
		return []byte(f.show), nil
	}
	return nil, nil
}

func testSpec(dir string) serviceSpec {
	data := filepath.Join(dir, "data dir")
	return serviceSpec{Label: "com.github.perishabledave.bosun-test", Bosund: "/opt/bosun/bin/bosund", Config: filepath.Join(dir, "bosun.yaml"),
		DataDir: data, LogFile: filepath.Join(data, "logs", "bosund.log")}
}

func TestLaunchdInstallLifecycle(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRunner{}
	var out bytes.Buffer
	c := &serviceCtl{goos: "darwin", home: filepath.Join(dir, "home"), uid: 501, spec: testSpec(dir), run: fr.run, out: &out,
		health: func(context.Context) (string, error) { return "v1.2.3", nil }}
	ctx := context.Background()
	if err := c.install(ctx); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(dir, "home", "Library", "LaunchAgents", "com.github.perishabledave.bosun-test.plist")
	b, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	for _, want := range []string{
		"<key>Label</key>\n\t<string>com.github.perishabledave.bosun-test</string>",
		"<string>/opt/bosun/bin/bosund</string>\n\t\t<string>--config</string>\n\t\t<string>" + filepath.Join(dir, "bosun.yaml") + "</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>\n\t<string>" + filepath.Join(dir, "data dir", "logs", "bosund.log") + "</string>",
		"<key>StandardErrorPath</key>",
		"<key>Umask</key>\n\t<integer>63</integer>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	for _, secret := range []string{"API_KEY", "TOKEN", "PASSWORD"} {
		if strings.Contains(plist, secret) {
			t.Errorf("plist mentions %s", secret)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "data dir", "logs")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log dir: %v %v", fi, err)
	}
	if got := strings.Join(fr.calls, "; "); got != "launchctl print gui/501/com.github.perishabledave.bosun-test; launchctl bootstrap gui/501 "+plistPath+"; launchctl kickstart gui/501/com.github.perishabledave.bosun-test" {
		t.Errorf("install calls = %s", got)
	}

	// Re-install unloads first.
	fr.calls = nil
	if err := c.install(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fr.calls, "; "), "launchctl bootout gui/501/com.github.perishabledave.bosun-test; launchctl print gui/501/com.github.perishabledave.bosun-test; launchctl bootstrap") {
		t.Errorf("reinstall calls = %v", fr.calls)
	}

	out.Reset()
	if err := c.status(ctx); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "running (pid 4242)") || !strings.Contains(s, "healthy, bosund v1.2.3") || !strings.Contains(s, "(installed)") {
		t.Errorf("status:\n%s", s)
	}

	fr.calls = nil
	if err := c.restart(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.start(ctx); err != nil {
		t.Fatal(err)
	}
	want := "launchctl print gui/501/com.github.perishabledave.bosun-test; launchctl kickstart -k gui/501/com.github.perishabledave.bosun-test; " +
		"launchctl print gui/501/com.github.perishabledave.bosun-test; launchctl bootout gui/501/com.github.perishabledave.bosun-test; launchctl print gui/501/com.github.perishabledave.bosun-test; " +
		"launchctl print gui/501/com.github.perishabledave.bosun-test; launchctl bootstrap gui/501 " + plistPath + "; launchctl kickstart gui/501/com.github.perishabledave.bosun-test"
	if got := strings.Join(fr.calls, "; "); got != want {
		t.Errorf("restart/stop/start calls =\n%s\nwant\n%s", got, want)
	}

	if got := strings.Join(c.logsCommand(50, true), " "); got != "tail -n 50 -F "+c.spec.LogFile {
		t.Errorf("logs = %s", got)
	}

	if err := c.uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist still present: %v", err)
	}
	if fr.loaded {
		t.Error("still loaded after uninstall")
	}
	if err := c.start(ctx); err == nil {
		t.Error("start after uninstall should fail")
	}
}

func TestLaunchdStateParsing(t *testing.T) {
	out := "gui/501/x = {\n\tactive count = 0\n\tstate = not running\n\tendpoints = {\n\t\t\"x\" = {\n\t\t\tstate = active\n\t\t}\n\t}\n\tlast exit code = 1\n}\n"
	c := &serviceCtl{goos: "darwin", uid: 501, spec: serviceSpec{Label: "x"},
		run: func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil }}
	if st, running := c.state(context.Background()); running || st != "not running, last exit code 1" {
		t.Errorf("state = %q %v", st, running)
	}
	if !strings.Contains(string(launchdPlist(testSpec(t.TempDir()))), "<key>KeepAlive</key>\n\t<true/>") {
		t.Error("KeepAlive must be a plain true (a dict defers the load-time start)")
	}
}

func TestSystemdUserUnit(t *testing.T) {
	dir := t.TempDir()
	fr := &fakeRunner{show: "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=777\n"}
	var out bytes.Buffer
	spec := testSpec(dir)
	spec.Label = "bosund"
	c := &serviceCtl{goos: "linux", home: filepath.Join(dir, "home"), uid: 1000, spec: spec, run: fr.run, out: &out}
	ctx := context.Background()
	if err := c.install(ctx); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(dir, "home", ".config", "systemd", "user", "bosund.service")
	b, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	for _, want := range []string{
		`ExecStart="/opt/bosun/bin/bosund" --config "` + filepath.Join(dir, "bosun.yaml") + `"`,
		"Restart=on-failure",
		"UMask=0077",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "User=") {
		t.Error("user unit must not set User=")
	}
	if got := strings.Join(fr.calls, "; "); got != "systemctl --user daemon-reload; systemctl --user enable --now bosund.service" {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(out.String(), "loginctl enable-linger") {
		t.Errorf("install output should mention lingering:\n%s", out.String())
	}
	if st, running := c.state(ctx); !running || st != "active (running), pid 777" {
		t.Errorf("state = %q %v", st, running)
	}
	if got := strings.Join(c.logsCommand(20, true), " "); got != "journalctl --user -u bosund.service -n 20 -f" {
		t.Errorf("logs = %s", got)
	}
	fr.calls = nil
	if err := c.uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fr.calls, "; "); got != "systemctl --user disable --now bosund.service; systemctl --user daemon-reload" {
		t.Errorf("uninstall calls = %s", got)
	}
}

func TestSystemdSystemUnit(t *testing.T) {
	dir := t.TempDir()
	spec := testSpec(dir)
	spec.Label, spec.System = "bosund", true
	unit := string(systemdUnit(spec))
	for _, want := range []string{"User=bosun", "Group=bosun", "WantedBy=multi-user.target", `ReadWritePaths="` + spec.DataDir + `"`} {
		if !strings.Contains(unit, want) {
			t.Errorf("system unit missing %q:\n%s", want, unit)
		}
	}
	// Writing /etc needs root; bosun never runs sudo itself.
	c := &serviceCtl{goos: "linux", euid: 1000, spec: spec, run: (&fakeRunner{}).run, out: &bytes.Buffer{}}
	if c.unitPath() != "/etc/systemd/system/bosund.service" {
		t.Errorf("path = %s", c.unitPath())
	}
	if err := c.install(context.Background()); err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Errorf("non-root system install: %v", err)
	}
}

func TestSystemdQuote(t *testing.T) {
	if got := systemdQuote(`/a b/"c"/100%/$HOME`); got != `"/a b/\"c\"/100%%/$$HOME"` {
		t.Errorf("quote = %s", got)
	}
}

func TestServicePrint(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("bosun service supports macOS and Linux")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bosun.yaml")
	if err := os.WriteFile(cfgPath, []byte("daemon: { data_dir: "+filepath.Join(dir, "data")+" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bosund := filepath.Join(dir, "bosund")
	if err := os.WriteFile(bosund, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, "", "--config", cfgPath, "service", "install", "--print", "--bosund", bosund, "--label", "com.github.perishabledave.bosun-print-test")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, bosund) || !strings.Contains(out, cfgPath) || !strings.Contains(out, "com.github.perishabledave.bosun-print-test") {
		t.Errorf("print output:\n%s", out)
	}
	if _, err := runCLI(t, "", "--config", cfgPath, "service", "status", "--label", "bad label"); err == nil {
		t.Error("bad label accepted")
	}
}

func TestLaunchdInstallRemovesLegacyAgent(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(agents, legacyLaunchdLabel+".plist")
	if err := os.WriteFile(legacy, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(dir)
	spec.Label = DefaultLaunchdLabel
	fr := &fakeRunner{loaded: true} // the legacy agent is running
	var out bytes.Buffer
	c := &serviceCtl{goos: "darwin", home: home, uid: 501, spec: spec, run: fr.run, out: &out}
	if err := c.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy plist still present: %v", err)
	}
	calls := strings.Join(fr.calls, "; ")
	if !strings.HasPrefix(calls, "launchctl print gui/501/dev.bosun.bosund; launchctl bootout gui/501/dev.bosun.bosund; ") ||
		!strings.Contains(calls, "launchctl bootstrap gui/501 "+filepath.Join(agents, DefaultLaunchdLabel+".plist")) {
		t.Errorf("calls = %s", calls)
	}
	if !strings.Contains(out.String(), "Removed the old agent dev.bosun.bosund") {
		t.Errorf("output:\n%s", out.String())
	}

	// A custom label leaves other agents alone.
	if err := os.WriteFile(legacy, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.spec.Label = "com.example.other"
	if err := c.install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("legacy plist removed for a custom label: %v", err)
	}
}
