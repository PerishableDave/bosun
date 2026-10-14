package config

import (
	"strings"
	"testing"
)

func TestBehindProxyValidation(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"non-loopback without tls or proxy", `daemon: { listen: "0.0.0.0:7777" }`, "requires daemon.tls.enabled"},
		{"non-loopback behind proxy", `daemon: { listen: "0.0.0.0:7777", behind_proxy: true }`, ""},
		{"proxy and tls", `daemon: { listen: "127.0.0.1:7777", behind_proxy: true, tls: { enabled: true, cert_file: /c, key_file: /k } }`, "mutually exclusive"},
		{"bad cidr", `daemon: { behind_proxy: true, trusted_proxies: ["nope"] }`, "trusted_proxies \"nope\""},
		{"proxies without behind_proxy", `daemon: { trusted_proxies: ["10.0.0.1"] }`, "only used with daemon.behind_proxy"},
		{"good list", `daemon: { behind_proxy: true, trusted_proxies: ["10.0.0.1", "100.64.0.0/10", "::1"] }`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestProxyNets(t *testing.T) {
	d := DaemonConfig{}
	if n, err := d.ProxyNets(); err != nil || n != nil {
		t.Fatalf("off: %v %v", n, err)
	}
	d.BehindProxy = true
	n, err := d.ProxyNets()
	if err != nil || len(n) != len(DefaultTrustedProxies) {
		t.Fatalf("defaults: %v %v", n, err)
	}
	d.TrustedProxies = []string{"100.101.102.103"}
	n, err = d.ProxyNets()
	if err != nil || len(n) != 1 || n[0].String() != "100.101.102.103/32" {
		t.Fatalf("single ip: %v %v", n, err)
	}
}
