package config

import (
	"strings"
	"testing"
	"time"
)

func TestDaemonTimezone(t *testing.T) {
	cfg, err := Parse([]byte("daemon: { timezone: Asia/Tokyo }\n"))
	if err != nil {
		t.Fatal(err)
	}
	loc, err := cfg.Daemon.Location()
	if err != nil || loc.String() != "Asia/Tokyo" {
		t.Errorf("Location = %v, %v", loc, err)
	}
	cfg, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if loc, err := cfg.Daemon.Location(); err != nil || loc != time.Local {
		t.Errorf("default Location = %v, %v", loc, err)
	}
	if _, err := Parse([]byte("daemon: { timezone: Mars/Olympus }\n")); err == nil || !strings.Contains(err.Error(), "daemon.timezone") {
		t.Errorf("bad timezone err = %v", err)
	}
}
