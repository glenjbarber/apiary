package main

// The walkthrough's terminal and file helpers.
//
// They live in their own file because they are the parts most likely
// to be wrong in a way that is not obvious: a secret read that echoes,
// a file written with the wrong mode, a JSON record that loses a
// field.

import (
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/term"
)

// readSecret reads a line from the terminal without echoing it.
//
// It uses golang.org/x/term rather than a hand-rolled termios ioctl
// dance, for two reasons. The hand-rolled version is the one that
// silently fails to disable echo on some terminals - and a secret read
// that echoes is worse than no secret read at all, because the
// operator believes it was hidden. And the ioctl constants are
// platform-specific: FreeBSD's TIOCGETA is Linux's TCGETS, so a
// hand-rolled version does not build everywhere, and the walkthrough
// is built on more than one platform.
//
// The terminal is restored to its previous state on every path out,
// including an error, so a failed read does not leave the operator's
// shell with echo disabled.
//
// If the terminal cannot be put into no-echo mode, the read REFUSES.
// There is no echoed fallback: the caller is told to use --token-file
// instead, and the token in that file is not on the screen either.
func readSecret(f *os.File) (string, error) {
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("the terminal is not interactive, so the value cannot be read without echoing it. Use --token-file instead")
	}
	previous, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("putting the terminal into no-echo mode: %w. Use --token-file instead", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_ = term.Restore(fd, previous)
			restored = true
		}
	}
	defer restore()

	var secret []byte
	buf := make([]byte, 1)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			switch buf[0] {
			case '\r', '\n':
				return string(secret), nil
			case 0x7f, 0x08: // DEL and BS
				if len(secret) > 0 {
					secret = secret[:len(secret)-1]
				}
			case 0x03: // Ctrl-C
				return "", fmt.Errorf("cancelled")
			default:
				secret = append(secret, buf[0])
			}
		}
		if readErr != nil {
			if len(secret) == 0 {
				return "", fmt.Errorf("reading from the terminal: %w", readErr)
			}
			return string(secret), nil
		}
	}
}

// decodeJSON decodes a JSON record.
func decodeJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// encodeJSON encodes a JSON record.
func encodeJSON(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the record: %w", err)
	}
	return append(data, '\n'), nil
}
