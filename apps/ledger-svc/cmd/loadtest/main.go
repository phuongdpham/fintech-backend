// Command loadtest seeds ledger-svc's account table for k6-driven load runs
// and emits a fixtures file that the k6 scenario consumes.
//
//	loadtest seed -dsn=$DATABASE_URL -tenants=10000 -accounts-per-tenant=100 \
//	              -fixtures=apps/ledger-svc/load/fixtures.json
//
// Why Go for seed and k6 for run.
//
// k6 owns load generation: open-loop scheduler, HdrHistogram percentiles,
// Prometheus remote-write, ramp DSL, distributed scaling — all things we
// would otherwise reinvent. The reason we don't run *seeding* in k6 is
// that the server's UUIDs and tenant strings are derived from a
// SHA-256-keyed scheme that has to match between seed and run; porting
// that to JS and keeping it in lock-step with Go would be its own
// long-term tax. Easier to seed in Go (which already imports the same
// types) and hand k6 a fixtures file as input.
//
// Seed is idempotent — re-running with the same -rng-seed produces the
// same UUIDs and the INSERT clauses use ON CONFLICT DO NOTHING.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch sub {
	case "seed":
		err = runSeed(ctx, args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "loadtest: unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `loadtest — ledger-svc load harness (seed side)

Subcommand:
  seed   Populate accounts and emit a fixtures file for the k6 runner.

Run the load itself with k6:
  k6 run apps/ledger-svc/load/k6/transfer.js \
      -e FIXTURES=apps/ledger-svc/load/fixtures.json -e ADDR=localhost:9090

Run "loadtest seed -h" for flags.
`)
}

// -----------------------------------------------------------------------------
// seed subcommand
// -----------------------------------------------------------------------------

type seedFlags struct {
	dsn               string
	tenants           int
	accountsPerTenant int
	currency          string
	rngSeed           uint64
	concurrency       int
	fixturesPath      string
}

func runSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	var f seedFlags
	fs.StringVar(&f.dsn, "dsn", os.Getenv("DATABASE_URL"), "postgres DSN; default $DATABASE_URL")
	fs.IntVar(&f.tenants, "tenants", 1000, "tenant cardinality")
	fs.IntVar(&f.accountsPerTenant, "accounts-per-tenant", 100, "accounts per tenant")
	fs.StringVar(&f.currency, "currency", "USD", "currency code (single)")
	fs.Uint64Var(&f.rngSeed, "rng-seed", 42, "deterministic RNG seed (same seed → same UUIDs)")
	fs.IntVar(&f.concurrency, "concurrency", 16, "parallel insert workers")
	fs.StringVar(&f.fixturesPath, "fixtures", "apps/ledger-svc/load/fixtures.json",
		"path to write the k6 fixtures file (use - for stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.dsn == "" {
		return errors.New("missing -dsn (or set DATABASE_URL)")
	}

	pool, err := pgxpool.New(ctx, f.dsn)
	if err != nil {
		return fmt.Errorf("pgxpool.New: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgxpool.Ping: %w", err)
	}

	total := f.tenants * f.accountsPerTenant
	fmt.Printf("seeding %d tenants × %d accounts = %d rows (currency=%s, concurrency=%d)\n",
		f.tenants, f.accountsPerTenant, total, f.currency, f.concurrency)

	jobs := make(chan accountSpec, f.concurrency*2)
	var wg sync.WaitGroup
	var inserted, skipped, failed atomic.Int64

	for range f.concurrency {
		wg.Go(func() {
			for spec := range jobs {
				_, err := pool.Exec(ctx,
					`INSERT INTO accounts (id, tenant_id, currency, status)
					 VALUES ($1, $2, $3, 'ACTIVE')
					 ON CONFLICT (id) DO NOTHING`,
					spec.id, spec.tenant, spec.currency,
				)
				switch {
				case err == nil:
					inserted.Add(1)
				case isCtxErr(err):
					return
				default:
					if strings.Contains(err.Error(), "duplicate") {
						skipped.Add(1)
					} else {
						failed.Add(1)
					}
				}
			}
		})
	}

	go func() {
		defer close(jobs)
		for ti := range f.tenants {
			tenant := fmt.Sprintf("tenant-%06d", ti)
			for ai := range f.accountsPerTenant {
				spec := accountSpec{
					id:       deterministicAccountID(f.rngSeed, tenant, ai),
					tenant:   tenant,
					currency: f.currency,
				}
				select {
				case <-ctx.Done():
					return
				case jobs <- spec:
				}
			}
		}
	}()

	wg.Wait()
	fmt.Printf("seed done: inserted=%d skipped=%d failed=%d\n",
		inserted.Load(), skipped.Load(), failed.Load())
	if failed.Load() > 0 {
		return fmt.Errorf("seed had %d failures (check connectivity / schema)", failed.Load())
	}

	if err := writeFixtures(f); err != nil {
		return fmt.Errorf("write fixtures: %w", err)
	}
	if f.fixturesPath != "-" {
		fmt.Printf("fixtures written to %s\n", f.fixturesPath)
	}
	return nil
}

// -----------------------------------------------------------------------------
// fixtures emission — k6 reads this
// -----------------------------------------------------------------------------

// fixtures is the JSON shape the k6 scenario expects. Kept flat on
// purpose so k6's JS can iterate without recursive descent. Tenants
// indexed by a parallel slice so k6 can sample by index without map
// iteration order randomness.
type fixtures struct {
	Currency        string                  `json:"currency"`
	Tenants         []string                `json:"tenants"`
	AccountsByIndex [][]string              `json:"accountsByIndex"` // accountsByIndex[i] is accounts for tenants[i]
	Meta            fixturesMeta            `json:"meta"`
}

type fixturesMeta struct {
	RNGSeed           uint64 `json:"rngSeed"`
	AccountsPerTenant int    `json:"accountsPerTenant"`
	TotalAccounts     int    `json:"totalAccounts"`
	HotShareHint      float64 `json:"hotShareHint"` // suggested fraction of writes to first 1% of accounts
}

func writeFixtures(f seedFlags) error {
	out := fixtures{
		Currency:        f.currency,
		Tenants:         make([]string, f.tenants),
		AccountsByIndex: make([][]string, f.tenants),
		Meta: fixturesMeta{
			RNGSeed:           f.rngSeed,
			AccountsPerTenant: f.accountsPerTenant,
			TotalAccounts:     f.tenants * f.accountsPerTenant,
			HotShareHint:      0.8,
		},
	}
	for ti := range f.tenants {
		tenant := fmt.Sprintf("tenant-%06d", ti)
		out.Tenants[ti] = tenant
		accs := make([]string, f.accountsPerTenant)
		for ai := range f.accountsPerTenant {
			accs[ai] = deterministicAccountID(f.rngSeed, tenant, ai).String()
		}
		out.AccountsByIndex[ti] = accs
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if f.fixturesPath == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	if dir := filepath.Dir(f.fixturesPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(f.fixturesPath, data, 0o644)
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

type accountSpec struct {
	id       uuid.UUID
	tenant   string
	currency string
}

// deterministicAccountID hashes (seed, tenant, accountIdx) into a v7-shaped
// UUID. Stateless and reproducible: caller can rebuild the same id from the
// same inputs at any point. The v7 version+variant bits keep PG happy with
// the UUID type; the timestamp portion is hash bytes — no chronological
// ordering, but seed doesn't need any (ON CONFLICT DO NOTHING handles
// re-run).
func deterministicAccountID(seed uint64, tenant string, idx int) uuid.UUID {
	h := sha256.New()
	var seedBuf [8]byte
	binary.BigEndian.PutUint64(seedBuf[:], seed)
	h.Write(seedBuf[:])
	h.Write([]byte(tenant))
	var idxBuf [8]byte
	binary.BigEndian.PutUint64(idxBuf[:], uint64(idx))
	h.Write(idxBuf[:])
	sum := h.Sum(nil)
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0F) | 0x70 // version 7
	u[8] = (u[8] & 0x3F) | 0x80 // variant 10xx
	return u
}

func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
