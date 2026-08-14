package console

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// scriptRW feeds scripted input one byte at a time and captures output. When the
// input is exhausted it returns io.EOF so the console's run loop returns.
type scriptRW struct {
	in  []byte
	pos int
	out strings.Builder
}

func (s *scriptRW) Read(p []byte) (int, error) {
	if s.pos >= len(s.in) {
		return 0, io.EOF
	}
	p[0] = s.in[s.pos]
	s.pos++
	return 1, nil
}

func (s *scriptRW) Write(p []byte) (int, error) {
	return s.out.Write(p)
}

// runInput drives a shell built from cfg with the given input and returns the
// captured output.
func runInput(cfg Config, input string) string {
	rw := &scriptRW{in: []byte(input)}
	New(cfg).Run(rw)
	return rw.out.String()
}

func echoCmd() Command {
	return Command{
		Name: "echo",
		Help: "echo arguments",
		Run:  func(args []string) (string, error) { return strings.Join(args, " "), nil },
	}
}

func TestCommandRuns(t *testing.T) {
	out := runInput(Config{Commands: []Command{echoCmd()}}, "echo hello world\n")
	if !strings.Contains(out, "hello world") {
		t.Fatalf("expected command output in %q", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	out := runInput(Config{}, "bogus\n")
	if !strings.Contains(out, "unknown command") {
		t.Fatalf("expected unknown-command message in %q", out)
	}
}

func TestHelpListsCommands(t *testing.T) {
	out := runInput(Config{Commands: []Command{echoCmd()}}, "help\n")
	for _, want := range []string{"echo", "echo arguments", "help", "reboot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help output missing %q: %q", want, out)
		}
	}
}

func TestCommandError(t *testing.T) {
	cfg := Config{Commands: []Command{{
		Name: "fail",
		Run:  func([]string) (string, error) { return "", errors.New("boom") },
	}}}
	out := runInput(cfg, "fail\n")
	if !strings.Contains(out, "error: boom") {
		t.Fatalf("expected error message in %q", out)
	}
}

func TestRebootInvokesReset(t *testing.T) {
	called := false
	out := runInput(Config{Reset: func() { called = true }}, "reboot\n")
	if !called {
		t.Fatalf("reset not invoked; output %q", out)
	}
	if !strings.Contains(out, "Rebooting") {
		t.Fatalf("expected reboot message in %q", out)
	}
}

func TestRebootNoResetConfigured(t *testing.T) {
	out := runInput(Config{}, "reboot\n")
	if !strings.Contains(out, "no reset function") {
		t.Fatalf("expected no-reset message in %q", out)
	}
}

func TestBackspaceEditing(t *testing.T) {
	// Type "echX", backspace (delete X), then "o hi": yields "echo hi".
	out := runInput(Config{Commands: []Command{echoCmd()}}, "echX\bo hi\n")
	if !strings.Contains(out, "hi") || strings.Contains(out, "unknown command") {
		t.Fatalf("backspace editing failed, output %q", out)
	}
}

func TestReservedNamesNotOverridable(t *testing.T) {
	// A command trying to shadow "help" must be ignored; built-in help wins.
	cfg := Config{Commands: []Command{{
		Name: "help",
		Run:  func([]string) (string, error) { return "SHADOW", nil },
	}}}
	out := runInput(cfg, "help\n")
	if strings.Contains(out, "SHADOW") {
		t.Fatalf("reserved name 'help' was overridden: %q", out)
	}
}

func powerCmd() Command {
	return Command{
		Name: "power",
		Help: "power control",
		Run:  func(args []string) (string, error) { return strings.Join(args, " "), nil },
		Complete: func(_ []string, _ string) []string {
			return []string{"on", "off", "status"}
		},
	}
}

func TestTabCompletesSingleCommand(t *testing.T) {
	// "ec" + Tab should complete to "echo " so "echo hi" runs.
	out := runInput(Config{Commands: []Command{echoCmd()}}, "ec\thi\n")
	if !strings.Contains(out, "hi") || strings.Contains(out, "unknown command") {
		t.Fatalf("command completion failed, output %q", out)
	}
}

func TestTabExtendsToCommonPrefix(t *testing.T) {
	// "pow" matches both "power" and "poweroff"; Tab must extend to the shared
	// "power" prefix (not list), so typing "off" then Enter runs "poweroff".
	off := Command{Name: "poweroff", Run: func([]string) (string, error) { return "OFF", nil }}
	out := runInput(Config{Commands: []Command{powerCmd(), off}}, "pow\toff\n")
	if !strings.Contains(out, "OFF") || strings.Contains(out, "unknown command") {
		t.Fatalf("common-prefix completion failed, output %q", out)
	}
}

func TestTabCompletesArgumentValue(t *testing.T) {
	// "power s" + Tab completes the argument to "status" via Command.Complete.
	out := runInput(Config{Commands: []Command{powerCmd()}}, "power s\t\n")
	if !strings.Contains(out, "status") {
		t.Fatalf("argument completion failed, output %q", out)
	}
}

func TestTabListsAmbiguousArguments(t *testing.T) {
	// "power o" + Tab is ambiguous ("on"/"off") and must list both.
	out := runInput(Config{Commands: []Command{powerCmd()}}, "power o\t")
	if !strings.Contains(out, "on") || !strings.Contains(out, "off") {
		t.Fatalf("ambiguous argument listing failed, output %q", out)
	}
}

func TestPromptAndBanner(t *testing.T) {
	out := runInput(Config{Prompt: "cairn# ", Banner: "WELCOME"}, "")
	if !strings.Contains(out, "WELCOME") {
		t.Fatalf("banner missing in %q", out)
	}
	if !strings.Contains(out, "cairn# ") {
		t.Fatalf("prompt missing in %q", out)
	}
}
