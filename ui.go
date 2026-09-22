package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var errInterrupt = errors.New("interrupted")

// UI owns the terminal. In interactive mode the bottom line is an editable
// prompt that is redrawn whenever a message is printed above it; otherwise
// lines are simply written out (for pipes, logs and services).
type UI struct {
	mu          sync.Mutex
	out         io.Writer
	fd          int
	interactive bool
	color       bool
	input       []rune
	online      bool
	status      map[string]string // e.g. "up" -> "↑ report.pdf 45%"
}

func NewUI(interactive bool) *UI {
	_, noColor := os.LookupEnv("NO_COLOR")
	return &UI{
		out:         os.Stdout,
		fd:          int(os.Stdin.Fd()),
		interactive: interactive,
		color:       interactive && !noColor && os.Getenv("TERM") != "dumb",
		status:      map[string]string{},
	}
}

// Print writes a (possibly multi-line) block above the prompt.
func (u *UI) Print(block string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.interactive {
		fmt.Fprint(u.out, "\r\x1b[K")
	}
	fmt.Fprintln(u.out, block)
	u.redraw()
}

func (u *UI) Printf(format string, args ...any) {
	u.Print(fmt.Sprintf(format, args...))
}

func (u *UI) SetOnline(online bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.online = online
	u.redraw()
}

func (u *UI) SetStatus(key, text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if text == "" {
		delete(u.status, key)
	} else {
		u.status[key] = text
	}
	u.redraw()
}

// redraw repaints the prompt line; callers hold u.mu.
func (u *UI) redraw() {
	if !u.interactive {
		return
	}
	var parts []string
	if !u.online {
		parts = append(parts, "connecting…")
	}
	for _, key := range []string{"up", "down"} {
		if s := u.status[key]; s != "" {
			parts = append(parts, s)
		}
	}
	plain := "> "
	styled := u.style("1;32", "> ")
	if len(parts) > 0 {
		info := "[" + strings.Join(parts, " · ") + "] "
		plain = info + plain
		styled = u.style("2", info) + styled
	}
	// show the tail of long input so the prompt never wraps
	input := u.input
	avail := termWidth(u.fd) - utf8.RuneCountInString(plain) - 1
	if avail < 1 {
		avail = 1
	}
	if len(input) > avail {
		input = input[len(input)-avail:]
	}
	fmt.Fprint(u.out, "\r\x1b[K"+styled+string(input))
}

// ReadLine reads one line of input with basic editing: backspace, Ctrl-U
// (clear), Ctrl-W (delete word), Ctrl-C (errInterrupt), Ctrl-D (io.EOF when
// the line is empty). Cursor keys are ignored.
func (u *UI) ReadLine(r *bufio.Reader) (string, error) {
	for {
		c, _, err := r.ReadRune()
		if err != nil {
			return "", err
		}
		u.mu.Lock()
		switch {
		case c == '\r' || c == '\n':
			line := string(u.input)
			u.input = u.input[:0]
			u.redraw()
			u.mu.Unlock()
			return line, nil
		case c == 3: // Ctrl-C
			u.mu.Unlock()
			return "", errInterrupt
		case c == 4: // Ctrl-D
			if len(u.input) == 0 {
				u.mu.Unlock()
				return "", io.EOF
			}
		case c == 127 || c == 8: // backspace
			if len(u.input) > 0 {
				u.input = u.input[:len(u.input)-1]
			}
		case c == 21: // Ctrl-U
			u.input = u.input[:0]
		case c == 23: // Ctrl-W
			i := len(u.input)
			for i > 0 && u.input[i-1] == ' ' {
				i--
			}
			for i > 0 && u.input[i-1] != ' ' {
				i--
			}
			u.input = u.input[:i]
		case c == 27: // escape sequence (arrow keys etc.): swallow it
			u.mu.Unlock()
			skipEscape(r)
			u.mu.Lock()
		case c == '\t':
			u.input = append(u.input, ' ')
		case c >= 32 && c != unicode.ReplacementChar:
			u.input = append(u.input, c)
		}
		u.redraw()
		u.mu.Unlock()
	}
}

func skipEscape(r *bufio.Reader) {
	c, _, err := r.ReadRune()
	if err != nil || (c != '[' && c != 'O') {
		return
	}
	for {
		c, _, err = r.ReadRune()
		if err != nil || (c >= 0x40 && c <= 0x7e) {
			return
		}
	}
}

func (u *UI) style(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

var nameColors = []string{"31", "33", "34", "35", "36", "91", "93", "94", "95", "96"}

func (u *UI) nameStyle(name string, own bool) string {
	if own {
		return u.style("1;32", name)
	}
	h := 0
	for _, r := range name {
		h = (h*31 + int(r)) % 1000003
	}
	return u.style("1;"+nameColors[h%len(nameColors)], name)
}

// clean removes control characters and bidi overrides from text that came
// from other people, so it cannot move the cursor or restyle the terminal.
func clean(s string, keepNewlines bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNewlines:
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r), r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

func formatBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", int64(n))
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

// splitArgs splits a command line on spaces, honouring '…', "…" and
// backslash escapes (what terminals insert when you drag a file in).
func splitArgs(s string) []string {
	var args []string
	var cur strings.Builder
	var quote rune
	inArg, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inArg = true, true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inArg = r, true
		case r == ' ':
			if inArg {
				args = append(args, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		args = append(args, cur.String())
	}
	return args
}
