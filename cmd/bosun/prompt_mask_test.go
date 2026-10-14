package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadMasked(t *testing.T) {
	for _, tc := range []struct {
		name, in, want, echo string
		err                  error
	}{
		{"plain", "secret\r", "secret", "******", nil},
		{"newline ends", "abc\n", "abc", "***", nil},
		{"backspace erases", "abcd\x7f\x7fe\r", "abe", "****\b \b\b \b*", nil},
		{"backspace on empty is ignored", "\x7fx\r", "x", "*", nil},
		{"ctrl-u clears", "abc\x15xy\r", "xy", "***\b \b\b \b\b \b**", nil},
		{"arrow keys ignored", "a\x1b[Db\r", "ab", "**", nil},
		{"control bytes ignored", "a\x01b\r", "ab", "**", nil},
		{"multibyte shows one star", "é!\r", "é!", "**", nil},
		{"multibyte backspace", "é\x7fx\r", "x", "*\b \b*", nil},
		{"ctrl-c interrupts", "ab\x03", "", "**", errInterrupted},
		{"ctrl-d on empty is EOF", "\x04", "", "", io.EOF},
		{"ctrl-d mid-line ignored", "a\x04b\r", "ab", "**", nil},
		{"eof with input returns it", "abc", "abc", "***", nil},
		{"spaces kept", "ab cd\r", "ab cd", "*****", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := readMasked(strings.NewReader(tc.in), &out)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Errorf("value = %q, want %q", got, tc.want)
			}
			if out.String() != tc.echo {
				t.Errorf("echo = %q, want %q", out.String(), tc.echo)
			}
		})
	}
}
