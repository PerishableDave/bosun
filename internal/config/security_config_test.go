package config

import "testing"

func TestTLSAndMasterKeyValidation(t *testing.T) {
	cases := []struct {
		name, yaml string
		ok         bool
	}{
		{"loopback plain http", "daemon: { listen: 127.0.0.1:7777 }", true},
		{"localhost plain http", "daemon: { listen: localhost:7777 }", true},
		{"ipv6 loopback plain http", "daemon: { listen: '[::1]:7777' }", true},
		{"wildcard requires tls", "daemon: { listen: 0.0.0.0:7777 }", false},
		{"ipv6 wildcard requires tls", "daemon: { listen: '[::]:7777' }", false},
		{"empty host requires tls", "daemon: { listen: ':7777' }", false},
		{"lan ip requires tls", "daemon: { listen: 192.168.1.10:7777 }", false},
		{"lan with tls", "daemon: { listen: 0.0.0.0:7777, tls: { enabled: true, cert_file: /c.pem, key_file: /k.pem } }", true},
		{"tls missing key", "daemon: { listen: 0.0.0.0:7777, tls: { enabled: true, cert_file: /c.pem } }", false},
		{"tls missing cert", "daemon: { tls: { enabled: true, key_file: /k.pem } }", false},
		{"loopback with tls", "daemon: { tls: { enabled: true, cert_file: /c.pem, key_file: /k.pem } }", true},
		{"master key default", "daemon: {}", true},
		{"master key auto", "daemon: { master_key: auto }", true},
		{"master key file", "daemon: { master_key: file }", true},
		{"master key keychain", "daemon: { master_key: keychain }", true},
		{"master key bogus", "daemon: { master_key: tpm }", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if (err == nil) != c.ok {
				t.Fatalf("Parse(%q) err = %v, want ok=%v", c.yaml, err, c.ok)
			}
		})
	}
}
