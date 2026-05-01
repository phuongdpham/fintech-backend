# ledger-svc load harness

Hybrid: Go binary owns deterministic seeding + fixtures emission; k6
owns the open-loop load run, percentile aggregation, and Prometheus
push.

## Why split this way

k6 is the right tool for load *generation* — open-loop scheduling
(`ramping-arrival-rate` paces by clock, not response), HdrHistogram
percentiles, Prometheus remote-write, distributed scaling via the k6
Operator if a single box runs out of NIC. We don't generate load in
Go because the homegrown ticker-based dispatch we'd have to write is
strictly worse than what k6 already does.

We don't *seed* in k6 because the server's account UUIDs are derived
from a SHA-256-keyed scheme that has to match between seed and run.
Porting that derivation to JS and keeping it in lock-step with Go
would be its own long-term tax. The Go seeder shares the same import
graph as the server; drift can't happen.

So: Go writes fixtures, k6 reads fixtures.

## Run flow

```sh
# 1. Bring up infra (PG + Redis + Redpanda + ledger-svc).
make stack-up

# 2. Bring up load sidecar (Prometheus on :9091, Grafana on :3001).
make -C apps/ledger-svc loadtest-infra-up

# 3. Seed accounts + emit fixtures.json. Idempotent.
make -C apps/ledger-svc loadtest-seed SEED_TENANTS=1000 SEED_ACCOUNTS=100

# 4. Run k6. Requires k6 ≥ v0.49 (built-in grpc).
make -C apps/ledger-svc loadtest-run

# 5. Open Grafana: http://localhost:3001  (anonymous Admin enabled)
```

## Tuning the ramp

The default ramp is in `k6/transfer.js` under `options.scenarios`:
1k → 4k → 8k → 12k → sustained 12k. Override the peak via env:

```sh
STAGE_PEAK=8000 make -C apps/ledger-svc loadtest-run
```

Edit the `stages` array directly for non-trivial profile changes.

## What the harness measures

- `transfer_latency_ms{kind=ok|err}` — Trend, end-to-end client-side.
- `transfer_ok_total`, `transfer_err_total{code=...}` — Counters,
  succeeded vs failed by gRPC code.
- `k6_vus`, `k6_vus_max` — VU pool occupancy. Saturation here means
  the open-loop dispatcher is starved; raise `preAllocatedVUs`.

Server-side metrics (pgxpool wait time, retry rate, admission queue
depth, etc.) come from ledger-svc's own `/metrics` once they're wired
in by issues 2/5/6/7. Grafana dashboards for those land alongside
those issues.

## Capturing pprof during a run

The load generator runs in its own process so its GC can't pollute
server pprof. Capture server-side profiles in a separate terminal:

```sh
go tool pprof -seconds=30 http://localhost:6060/debug/pprof/profile
go tool pprof -seconds=30 http://localhost:6060/debug/pprof/heap
go tool pprof http://localhost:6060/debug/pprof/mutex
```

(pprof endpoint exposure on the server is part of issue 2's
instrumentation work.)

## Cleanup

```sh
make -C apps/ledger-svc loadtest-infra-down
# Optional: drop the Grafana/Prom volumes for a clean slate.
docker volume rm fintech-backend_prom-load-data fintech-backend_grafana-load-data
```
