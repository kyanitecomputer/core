// Package console implements an interactive serial / SSH command console shared
// by the Kyanite device runtimes (vein, cairn).
//
// The console is deliberately hardware- and domain-agnostic: it owns the line
// editor, history, tab completion, dispatch and I/O framing, and nothing else.
// Everything domain-specific — switch verbs, BMC verbs, register pokes — is
// supplied by the caller as a set of [Command] values. This mirrors the
// inversion used elsewhere in core: the neutral layer depends only on small
// interfaces (here, io.Reader/io.Writer and a command function), and the target
// injects the behaviour.
//
// # Transport
//
// Run drives the console over any io.ReadWriter. Reads are expected to block
// until at least one byte is available (an SSH channel does this natively; for a
// polled UART the board wraps it in a small blocking adapter). Writes normalise
// bare LF to CR-LF so command output using "\n" renders correctly on a raw
// serial terminal.
//
// # Features
//
//   - Line editing: mid-line insert/delete, left/right arrows, Home/End
//     (Ctrl-A/Ctrl-E), Backspace, Delete, Ctrl-U (kill line), Ctrl-C (interrupt)
//   - History: last 16 commands (up/down arrows via VT100 sequences)
//   - Tab completion on command names (built-ins and registered commands)
//   - Built-in commands: help / ?, reboot
//
// The package is stdlib-only and host-testable: Run operates purely on
// io.ReadWriter, so it can be driven with an in-memory pipe in a unit test.
package console

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	historySize = 16
	maxLineLen  = 256

	defaultPrompt = "> "
)

// Command is a console command supplied by the caller.
//
// Run receives the argument list (the whitespace-separated words after the
// command name) and returns text to print and/or an error. A nil error prints
// Output verbatim (CR-LF normalised); a non-nil error prints "error: <err>".
// Output and error are not mutually exclusive: a command may return partial
// output alongside an error.
//
// A handler should print its own usage (as Output) when invoked with
// missing or invalid arguments.
type Command struct {
	// Name is the word that invokes the command (e.g. "show", "md").
	Name string
	// Help is a one-line description shown by the built-in help command.
	Help string
	// Run executes the command.
	Run func(args []string) (string, error)
}

// Config configures a [Shell].
type Config struct {
	// Prompt is the string printed before each input line. Defaults to "> ".
	Prompt string
	// Banner, if non-empty, is printed once when the console starts.
	Banner string
	// Reset, if non-nil, is invoked by the built-in "reboot" command. It is
	// expected not to return; if it does, the console reports the anomaly.
	Reset func()
	// Commands are the caller-supplied commands. Names must not collide with
	// the built-ins ("help", "?", "reboot"); colliding entries are ignored.
	Commands []Command
}

// Shell is an interactive console instance. Construct it with [New] and drive it
// with [Shell.Run].
type Shell struct {
	prompt string
	banner string
	reset  func()

	cmds  map[string]Command
	names []string // sorted command names (built-ins + registered) for help/completion

	w io.Writer // active output, set for the duration of Run

	history [historySize]string
	histIdx int // next write position (ring buffer)
	histLen int // number of entries stored
}

// reserved names are handled by the engine and cannot be overridden.
var reserved = map[string]bool{"help": true, "?": true, "reboot": true}

// New creates a Shell from cfg.
func New(cfg Config) *Shell {
	s := &Shell{
		prompt: cfg.Prompt,
		banner: cfg.Banner,
		reset:  cfg.Reset,
		cmds:   make(map[string]Command, len(cfg.Commands)),
	}
	if s.prompt == "" {
		s.prompt = defaultPrompt
	}
	for _, c := range cfg.Commands {
		if c.Name == "" || reserved[c.Name] || c.Run == nil {
			continue
		}
		s.cmds[c.Name] = c
	}
	s.names = append(s.names, "help", "reboot")
	for n := range s.cmds {
		s.names = append(s.names, n)
	}
	sort.Strings(s.names)
	return s
}

// Run drives the console over rw until a read returns an error (e.g. the
// underlying connection closes), then returns. Call it from its own goroutine.
func (s *Shell) Run(rw io.ReadWriter) {
	s.w = rw
	defer func() { s.w = nil }()
	s.runLoop(rw)
}

// runLoop is the character-processing loop. r must return input a byte at a
// time (only the first byte of each Read result is consumed).
func (s *Shell) runLoop(r io.Reader) {
	if s.banner != "" {
		s.writeln(s.banner)
	}
	s.writeln("Type 'help' for commands.")
	s.writePrompt()

	var (
		line    []byte
		pos     int // cursor position within line (0..len(line))
		histPos = -1
		escBuf  []byte
		buf     [1]byte
	)

	for {
		n, err := r.Read(buf[:])
		if n == 0 {
			if err != nil {
				return // connection closed or fatal read error
			}
			continue
		}
		c := buf[0]

		// VT100 escape sequences: ESC '[' <params> <final>, final a letter or '~'.
		if len(escBuf) > 0 {
			escBuf = append(escBuf, c)
			if len(escBuf) == 2 && c != '[' {
				escBuf = escBuf[:0] // not a CSI sequence we handle
				continue
			}
			final := c
			if len(escBuf) < 3 || !(final >= 'A' && final <= 'Z' || final == '~') {
				continue // still collecting
			}
			switch {
			case final == 'A': // up arrow — history previous
				line, histPos = s.histUp(line, histPos)
				pos = len(line)
			case final == 'B': // down arrow — history next
				line, histPos = s.histDown(line, histPos)
				pos = len(line)
			case final == 'C': // right arrow
				if pos < len(line) {
					pos++
					s.refresh(line, pos)
				}
			case final == 'D': // left arrow
				if pos > 0 {
					pos--
					s.refresh(line, pos)
				}
			case final == 'H' || (escBuf[2] == '1' && final == '~'): // Home
				pos = 0
				s.refresh(line, pos)
			case final == 'F' || (escBuf[2] == '4' && final == '~'): // End
				pos = len(line)
				s.refresh(line, pos)
			case escBuf[2] == '3' && final == '~': // Delete (forward)
				if pos < len(line) {
					line = append(line[:pos], line[pos+1:]...)
					s.refresh(line, pos)
				}
			}
			escBuf = escBuf[:0]
			continue
		}

		switch c {
		case 0x1b: // ESC — start of escape sequence
			escBuf = append(escBuf[:0], c)

		case '\r', '\n': // Enter
			s.writeln("")
			input := strings.TrimSpace(string(line))
			if input != "" {
				s.addHistory(input)
				s.dispatch(input)
			}
			line = line[:0]
			pos = 0
			histPos = -1
			s.writePrompt()

		case 0x7f, '\b': // Backspace / DEL — delete char before cursor
			if pos > 0 {
				line = append(line[:pos-1], line[pos:]...)
				pos--
				s.refresh(line, pos)
			}

		case 0x15: // Ctrl-U — kill whole line
			line = line[:0]
			pos = 0
			s.refresh(line, pos)

		case 0x01: // Ctrl-A — move to start of line
			pos = 0
			s.refresh(line, pos)

		case 0x05: // Ctrl-E — move to end of line
			pos = len(line)
			s.refresh(line, pos)

		case 0x03: // Ctrl-C — interrupt current line
			s.writeln("^C")
			line = line[:0]
			pos = 0
			histPos = -1
			s.writePrompt()

		case '\t': // Tab — complete command
			line = s.tabComplete(line)
			pos = len(line)

		default:
			if c >= 0x20 && c < 0x7f && len(line) < maxLineLen {
				line = append(line, 0)
				copy(line[pos+1:], line[pos:]) // shift tail right
				line[pos] = c
				pos++
				s.refresh(line, pos)
			}
		}
	}
}

// refresh redraws the current input line in place and positions the cursor at
// pos: CR to column 0, reprint prompt+line, erase to end of line (VT100 EL),
// then move the cursor left to pos.
func (s *Shell) refresh(line []byte, pos int) {
	s.write("\r")
	s.write(s.prompt)
	s.write(string(line))
	s.write("\x1b[K")
	for i := len(line); i > pos; i-- {
		s.write("\b")
	}
}

// dispatch parses and executes a command line.
func (s *Shell) dispatch(input string) {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return
	}
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "help", "?":
		s.cmdHelp()
	case "reboot":
		s.cmdReboot()
	default:
		c, ok := s.cmds[cmd]
		if !ok {
			s.writef("unknown command: %q — type 'help'\n", cmd)
			return
		}
		out, err := c.Run(args)
		if out != "" {
			s.write(out)
			if !strings.HasSuffix(out, "\n") {
				s.write("\n")
			}
		}
		if err != nil {
			s.writef("error: %v\n", err)
		}
	}
}

func (s *Shell) cmdHelp() {
	s.writeln("Commands:")
	for _, n := range s.names {
		switch n {
		case "help":
			s.writeln("  help / ?              — show this help")
		case "reboot":
			s.writeln("  reboot                — restart the device")
		default:
			c := s.cmds[n]
			if c.Help != "" {
				s.writef("  %-20s — %s\n", c.Name, c.Help)
			} else {
				s.writef("  %s\n", c.Name)
			}
		}
	}
}

func (s *Shell) cmdReboot() {
	if s.reset == nil {
		s.writeln("[console] no reset function configured — power cycle manually")
		return
	}
	s.writeln("Rebooting...")
	time.Sleep(100 * time.Millisecond)
	s.reset()
	s.writeln("[console] reset returned unexpectedly — power cycle manually")
}

// --- history ----------------------------------------------------------------

func (s *Shell) addHistory(line string) {
	s.history[s.histIdx%historySize] = line
	s.histIdx++
	if s.histLen < historySize {
		s.histLen++
	}
}

func (s *Shell) histUp(line []byte, pos int) ([]byte, int) {
	if pos < 0 {
		pos = 0
	} else if pos < s.histLen-1 {
		pos++
	} else {
		return line, pos
	}
	entry := s.history[(s.histIdx-1-pos+historySize)%historySize]
	return s.replaceLine(line, entry), pos
}

func (s *Shell) histDown(line []byte, pos int) ([]byte, int) {
	if pos <= 0 {
		return s.replaceLine(line, ""), -1
	}
	pos--
	entry := s.history[(s.histIdx-1-pos+historySize)%historySize]
	return s.replaceLine(line, entry), pos
}

func (s *Shell) replaceLine(old []byte, newLine string) []byte {
	nl := []byte(newLine)
	s.refresh(nl, len(nl))
	return nl
}

// --- tab completion ---------------------------------------------------------

func (s *Shell) tabComplete(line []byte) []byte {
	input := string(line)
	var matches []string
	for _, c := range s.names {
		if strings.HasPrefix(c, input) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		s.write("\a") // bell
	case 1:
		completed := []byte(matches[0] + " ")
		s.refresh(completed, len(completed))
		return completed
	default:
		s.writeln("")
		s.writeln(strings.Join(matches, "  "))
		s.refresh(line, len(line))
	}
	return line
}

// --- I/O helpers ------------------------------------------------------------

// write sends str to the active output, normalising bare LF to CR-LF so
// multi-line command output (which uses "\n") renders correctly on a raw serial
// terminal. Existing CR-LF is preserved (not doubled).
func (s *Shell) write(str string) {
	if s.w == nil {
		return
	}
	if strings.IndexByte(str, '\n') >= 0 {
		str = strings.ReplaceAll(str, "\r\n", "\n")
		str = strings.ReplaceAll(str, "\n", "\r\n")
	}
	_, _ = s.w.Write([]byte(str))
}

func (s *Shell) writeln(str string) {
	s.write(str)
	s.write("\r\n")
}

func (s *Shell) writef(format string, args ...any) {
	s.write(fmt.Sprintf(format, args...))
}

func (s *Shell) writePrompt() {
	s.write(s.prompt)
}
