package observability

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"

	"github.com/KimMachineGun/automemlimit/memlimit"
)

// ApplyRuntimeLimits sets GOMEMLIMIT and GOGC at process start.
//
// GOMEMLIMIT: derived from the cgroup memory limit (Linux containers,
// detected via memlimit.FromCgroup) at 90% of the container's hard
// limit. Honors the GOMEMLIMIT env var if set — that's how operators
// override our default for local-dev or for non-containerized runs.
// On non-cgroup hosts the call is a no-op and GOMEMLIMIT stays at the
// Go default (unlimited heap).
//
// Why 0.9, not 1.0: the cgroup limit is a hard ceiling — exceed it and
// the kernel OOM-kills. Go's GC reacts to GOMEMLIMIT by spending more
// CPU on collection as it approaches the limit; we want the assist
// pressure to ramp before the OOM hammer falls.
//
// GOGC: defaults to 200 (twice the stdlib default of 100). At 10K TPS
// the lower default triggers GC more often than necessary; raising it
// trades steady-state heap for fewer GC cycles. Operators can override
// via the GOGC env var.
//
// Both decisions are logged so an operator reading boot output knows
// what the runtime is actually doing.
func ApplyRuntimeLimits(log *slog.Logger) {
	// GOMEMLIMIT — apply automemlimit only when GOMEMLIMIT env is unset;
	// memlimit.SetGoMemLimitWithOpts honors the env, but we want the
	// log line to show the resulting state regardless of source.
	limit, err := memlimit.SetGoMemLimitWithOpts(
		memlimit.WithRatio(0.9),
		memlimit.WithProvider(memlimit.FromCgroup),
	)
	switch {
	case err == nil && limit > 0:
		log.Info("GOMEMLIMIT applied from cgroup",
			slog.String("bytes", humanBytes(limit)),
			slog.Float64("ratio", 0.9),
		)
	case err != nil:
		// Non-cgroup hosts return a typed error; that's expected when
		// running outside a container. Don't escalate — it's a hint,
		// not a fault.
		log.Info("GOMEMLIMIT not applied (no cgroup detected)",
			slog.Any("err", err),
		)
	default:
		// Limit==0 means cgroup reports "unlimited" — also fine.
		log.Info("GOMEMLIMIT not applied (cgroup unlimited)")
	}

	// GOGC — apply our default unless the operator overrode via env.
	// The env-provided value already takes effect at process start;
	// SetGCPercent is the way to apply our default *if* it isn't set.
	if _, set := os.LookupEnv("GOGC"); set {
		log.Info("GOGC applied from environment")
	} else {
		const defaultGOGC = 200
		old := debug.SetGCPercent(defaultGOGC)
		log.Info("GOGC applied",
			slog.Int("from", old),
			slog.Int("to", defaultGOGC),
		)
	}
}

// humanBytes renders a byte count as KiB / MiB / GiB. Used for boot
// log readability; not a hot-path formatter.
func humanBytes(n int64) string {
	const (
		_ = 1 << (10 * iota)
		kib
		mib
		gib
	)
	switch {
	case n >= gib:
		return strconv.FormatFloat(float64(n)/gib, 'f', 2, 64) + "GiB"
	case n >= mib:
		return strconv.FormatFloat(float64(n)/mib, 'f', 2, 64) + "MiB"
	case n >= kib:
		return strconv.FormatFloat(float64(n)/kib, 'f', 2, 64) + "KiB"
	default:
		return fmt.Sprintf("%dB", n)
	}
}
