package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// errInterrupted is returned when the user presses Ctrl-C at a masked prompt.
var errInterrupted = errors.New("interrupted")

// readMasked reads one line from in (which must already be in raw mode when
// it is a terminal) and echoes one '*' per character to out, so the user can
// see that keystrokes and pastes are arriving without revealing the secret.
//
// Keys: Enter finishes; Backspace/Delete erases one character; Ctrl-U clears
// the line; Ctrl-C returns errInterrupted; Ctrl-D on an empty line is EOF.
// Other control bytes and escape sequences (arrow keys) are ignored.
func readMasked(in io.Reader, out io.Writer) (string, error) {
	br := bufio.NewReader(in)
	var buf []byte // the secret, as UTF-8
	erase := func() {
		if len(buf) == 0 {
			return
		}
		_, size := utf8.DecodeLastRune(buf)
		buf = buf[:len(buf)-size]
		fmt.Fprint(out, "\b \b")
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
		switch b {
		case '\r', '\n':
			return string(buf), nil
		case 0x03: // Ctrl-C
			return "", errInterrupted
		case 0x04: // Ctrl-D
			if len(buf) == 0 {
				return "", io.EOF
			}
		case 0x7f, 0x08: // Delete, Backspace
			erase()
		case 0x15: // Ctrl-U: clear the line
			for len(buf) > 0 {
				erase()
			}
		case 0x1b: // escape sequence (arrow keys etc.): skip it
			skipEscape(br)
		default:
			if b < 0x20 {
				continue // other control bytes
			}
			buf = append(buf, b)
			// Echo once per completed rune so multi-byte characters show one '*'.
			if utf8.FullRune(buf[lastRuneStart(buf):]) {
				fmt.Fprint(out, "*")
			}
		}
	}
}

// lastRuneStart returns the index where the (possibly incomplete) last rune
// in buf begins.
func lastRuneStart(buf []byte) int {
	i := len(buf) - 1
	for i > 0 && !utf8.RuneStart(buf[i]) {
		i--
	}
	return i
}

// skipEscape consumes a CSI/SS3 escape sequence after ESC has been read.
func skipEscape(br *bufio.Reader) {
	b, err := br.ReadByte()
	if err != nil {
		return
	}
	if b != '[' && b != 'O' {
		return
	}
	for {
		c, err := br.ReadByte()
		if err != nil || (c >= 0x40 && c <= 0x7e) {
			return // final byte of the sequence
		}
	}
}
