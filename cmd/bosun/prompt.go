package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// prompter reads interactive answers. Passwords are read without echo when
// input is a terminal; otherwise they are read as plain lines (for scripting).
type prompter struct {
	in  io.Reader
	br  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: in, br: bufio.NewReader(in), out: out}
}

// line prompts for a line, returning def if the answer is empty.
func (p *prompter) line(label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", label)
	}
	s, err := p.br.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && s != "") {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// password prompts for a secret. On a terminal each typed character is shown
// as '*' (so the user can see input arriving) and the secret itself is never
// echoed; otherwise it is read as a plain line (for scripting).
func (p *prompter) password(label string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", label)
	if f, ok := p.in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		state, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", label, err)
		}
		s, err := readMasked(f, p.out)
		_ = term.Restore(int(f.Fd()), state)
		fmt.Fprintln(p.out)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", label, err)
		}
		return s, nil
	}
	s, err := p.br.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && s != "") {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	return strings.TrimRight(s, "\r\n"), nil
}
