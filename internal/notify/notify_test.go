package notify

import (
	"strings"
	"testing"
)

func TestArgs_Timeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout int
		wantMS  string
	}{
		{"seconds converted to milliseconds", 30, "30000"},
		{"negative never expires", -1, "0"},
		{"zero passes through as never expires", 0, "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := &Notifier{enabled: true, bin: "notify-send", timeoutSec: tc.timeout}
			args := n.args("smsd", "hello", Options{})
			got := flagValue(t, args, "-t")
			if got != tc.wantMS {
				t.Errorf("-t = %q, want %q", got, tc.wantMS)
			}
			if args[len(args)-2] != "smsd" || args[len(args)-1] != "hello" {
				t.Errorf("title/body must stay last, got %q", strings.Join(args, " "))
			}
		})
	}
}

func TestArgs_StickyReplace(t *testing.T) {
	n := &Notifier{enabled: true, bin: "notify-send", timeoutSec: 30}
	args := n.args("Incoming call", "Alice", Options{Icon: "call-start", Sticky: true, Replace: 42})
	if got := flagValue(t, args, "-t"); got != "0" {
		t.Errorf("sticky -t = %q, want 0", got)
	}
	if got := flagValue(t, args, "-r"); got != "42" {
		t.Errorf("-r = %q, want 42", got)
	}
	if got := flagValue(t, args, "-i"); got != "call-start" {
		t.Errorf("-i = %q, want call-start", got)
	}
	if args[len(args)-2] != "Incoming call" || args[len(args)-1] != "Alice" {
		t.Errorf("title/body must stay last, got %q", strings.Join(args, " "))
	}
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("flag %s not found in %q", flag, strings.Join(args, " "))
	return ""
}
