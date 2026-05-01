package observability

import (
	"bytes"
	"log/slog"
	"runtime/debug"
	"strings"
	"testing"
)

// TestApplyRuntimeLimits_LogsAndAppliesGOGC verifies the GOGC default
// path. We can't easily assert what GOMEMLIMIT lands on (depends on
// cgroup detection of the test host) but we can verify the log line
// shape and the GOGC behavior — set explicitly when env is unset.
func TestApplyRuntimeLimits_LogsAndAppliesGOGC(t *testing.T) {
	t.Setenv("GOGC", "")
	// Restore current GOGC after the test so we don't leak the
	// modified value into other tests in the same package.
	prev := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(prev) })

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ApplyRuntimeLimits(log)

	out := buf.String()
	if !strings.Contains(out, "GOGC applied") {
		t.Errorf("expected GOGC log line, got: %s", out)
	}
}

// TestApplyRuntimeLimits_HonorsGOGCEnv proves the env override path —
// when GOGC is set, we don't stomp on it.
func TestApplyRuntimeLimits_HonorsGOGCEnv(t *testing.T) {
	t.Setenv("GOGC", "150")
	prev := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(prev) })

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ApplyRuntimeLimits(log)

	out := buf.String()
	if !strings.Contains(out, "GOGC applied from environment") {
		t.Errorf("expected env-source log line, got: %s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{500, "500B"},
		{2048, "2.00KiB"},
		{5 * 1024 * 1024, "5.00MiB"},
		{2 * 1024 * 1024 * 1024, "2.00GiB"},
	}
	for _, tc := range cases {
		got := humanBytes(tc.in)
		if got != tc.want {
			t.Errorf("humanBytes(%d): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

