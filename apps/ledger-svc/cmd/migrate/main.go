// Command migrate applies golang-migrate-formatted SQL files against
// the ledger-svc Postgres database.
//
// Usage:
//
//	migrate -dsn "$DATABASE_URL" up
//	migrate -dsn "$DATABASE_URL" down 1
//	migrate -dsn "$DATABASE_URL" version
//	migrate -dsn "$DATABASE_URL" force <version>
//
// Config precedence: -dsn flag → env DATABASE_URL → .env file
// (root + apps/ledger-svc/.env, dev only). The migrations directory
// defaults to ./migrations relative to the working directory; override
// with -path. Designed for k8s init-containers and the root Makefile's
// stack-up flow alike.
//
// migrate intentionally does NOT call config.Load — it doesn't need
// Redis, Kafka, or any other service config, and forcing those required
// fields would mean ad-hoc invocations need bogus values. It DOES reuse
// config.LoadDotenv so .env layering is consistent across binaries.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"

	// Anonymous imports register the database + source drivers with the
	// migrate library. Removing either breaks `migrate.New(...)` at runtime.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/phuongdpham/fintech/apps/ledger-svc/internal/config"
)

func main() {
	// .env layering BEFORE flag.Parse() so the flag default reflects
	// what's in the file (otherwise the default-value capture below
	// happens against the raw os.Environ, missing the .env values).
	config.LoadDotenv("apps/ledger-svc/.env")

	var (
		dsn  = flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres DSN")
		path = flag.String("path", "./migrations", "migrations directory (file:// path or absolute)")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage: migrate [flags] <command> [args]

Commands:
  up                     apply all pending migrations
  down [n]               revert the last n migrations (default 1)
  version                print current schema version
  force <version>        mark <version> as the current state (recovery only)

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *dsn == "" {
		fail("DATABASE_URL (or -dsn) is required")
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	m, err := migrate.New(toFileURL(*path), normalizeDSN(*dsn))
	if err != nil {
		fail("init migrate: " + err.Error())
	}
	defer m.Close()

	switch args[0] {
	case "up":
		runErr(m.Up())
	case "down":
		n := 1
		if len(args) > 1 {
			parsed, perr := strconv.Atoi(args[1])
			if perr != nil || parsed <= 0 {
				fail("down: <n> must be a positive integer")
			}
			n = parsed
		}
		runErr(m.Steps(-n))
	case "version":
		v, dirty, vErr := m.Version()
		if errors.Is(vErr, migrate.ErrNilVersion) {
			fmt.Println("no migrations applied yet")
			return
		}
		if vErr != nil {
			fail("version: " + vErr.Error())
		}
		fmt.Printf("version=%d dirty=%v\n", v, dirty)
	case "force":
		if len(args) < 2 {
			fail("force: <version> required")
		}
		v, perr := strconv.Atoi(args[1])
		if perr != nil {
			fail("force: <version> must be an integer")
		}
		if err := m.Force(v); err != nil {
			fail("force: " + err.Error())
		}
		fmt.Printf("forced version=%d\n", v)
	default:
		fail("unknown command: " + args[0])
	}
}

// runErr handles the migrate idiom where ErrNoChange is returned when
// the schema is already at the target version — that's success, not failure.
func runErr(err error) {
	if err == nil || errors.Is(err, migrate.ErrNoChange) {
		fmt.Println("ok")
		return
	}
	fail(err.Error())
}

// toFileURL converts a plain path into the file:// URL the source driver
// expects. Already-prefixed inputs pass through unchanged.
func toFileURL(p string) string {
	if len(p) >= 7 && p[:7] == "file://" {
		return p
	}
	return "file://" + p
}

// normalizeDSN ensures the postgres:// URL has sslmode set. Without it,
// many self-hosted dev clusters (including our docker-compose Postgres)
// reject the handshake. Caller can override by setting sslmode in their URL.
func normalizeDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "migrate:", msg)
	os.Exit(1)
}
