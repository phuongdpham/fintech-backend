// transfer.js — k6 scenario driving Transfer RPCs against ledger-svc.
//
// Topology: Go cmd/loadtest seeds accounts and emits a fixtures file;
// this scenario reads it, picks tenant + accounts (Pareto-skewed: 80% of
// writes target the first 1% of accounts within a tenant), and issues
// Transfer RPCs at the rates declared in `scenarios`.
//
// Why constant-arrival-rate (open-loop). It paces requests by clock
// rather than waiting for the previous response — under server-side
// stalls, k6 keeps issuing at the target rate, queueing internally. That
// gives us the honest "what does p99 do at 10K TPS" measurement instead
// of the homegrown closed-loop fiction where p99 is artificially capped
// by VU count.
//
// Run:
//   k6 run apps/ledger-svc/load/k6/transfer.js \
//       -e FIXTURES=apps/ledger-svc/load/fixtures.json \
//       -e ADDR=localhost:9090 \
//       -e PROTO_DIR=shared/proto

import grpc from 'k6/net/grpc';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import exec from 'k6/execution';

// ---- Inputs ----------------------------------------------------------------

const FIXTURES_PATH = __ENV.FIXTURES || 'apps/ledger-svc/load/fixtures.json';
const ADDR          = __ENV.ADDR     || 'localhost:9090';
const PROTO_DIR     = __ENV.PROTO_DIR || 'shared/proto';
const PROTO_FILE    = 'fintech/ledger/v1/ledger.proto';

const fixtures = JSON.parse(open(`../../../../${FIXTURES_PATH}`));

// ---- gRPC client (one per VU) ----------------------------------------------

const client = new grpc.Client();
client.load([`../../../../${PROTO_DIR}`], PROTO_FILE);

// ---- Metrics ---------------------------------------------------------------

const transferLatency = new Trend('transfer_latency_ms', true);
const transferOK      = new Counter('transfer_ok_total');
const transferErr     = new Counter('transfer_err_total');

// ---- Scenarios -------------------------------------------------------------
//
// Ramp from 1k → 12k RPS. Tune via env if you want a different shape:
//   STAGE_PEAK=8000 STAGE_PEAK_DURATION=10m
//
// preAllocatedVUs is sized so the worst-case stage doesn't starve VUs at
// open-loop dispatch. Rule of thumb: VUs ≥ peak_RPS × p99_seconds × 1.5.

const peak = parseInt(__ENV.STAGE_PEAK || '12000', 10);

export const options = {
  scenarios: {
    transfer_ramp: {
      executor: 'ramping-arrival-rate',
      startRate: 100,
      timeUnit: '1s',
      preAllocatedVUs: Math.max(200, Math.floor(peak * 0.05)),
      maxVUs: Math.max(500, Math.floor(peak * 0.1)),
      stages: [
        { target: 1000,  duration: '2m' },
        { target: 4000,  duration: '5m' },
        { target: 8000,  duration: '10m' },
        { target: peak,  duration: '8m' },
        { target: peak,  duration: '5m' }, // sustained at peak
      ],
    },
  },
  thresholds: {
    // Bail loud if p99 explodes past the SLO; the run continues but
    // the threshold failure is visible in the summary.
    'transfer_latency_ms{kind:ok}': ['p(99)<200'],
    'transfer_err_total':           ['count<1000'],
  },
};

// ---- VU lifecycle ----------------------------------------------------------

export function setup() {
  // VUs share one client; connect once per VU (handled in default()).
  return { tenants: fixtures.tenants.length, accountsPerTenant: fixtures.meta.accountsPerTenant };
}

let connected = false;

export default function (data) {
  if (!connected) {
    client.connect(ADDR, { plaintext: true });
    connected = true;
  }

  const ti = randInt(data.tenants);
  const tenant = fixtures.tenants[ti];
  const accounts = fixtures.accountsByIndex[ti];

  let from = paretoIdx(data.accountsPerTenant);
  let to = paretoIdx(data.accountsPerTenant);
  if (to === from) {
    to = (from + 1) % data.accountsPerTenant;
  }

  const idemKey = `lt-${exec.scenario.iterationInTest}-${exec.vu.idInTest}`;
  const req = {
    idempotency_key: idemKey,
    from_account_id: accounts[from],
    to_account_id:   accounts[to],
    amount:          '1.0000',
    currency:        fixtures.currency,
  };

  const t0 = Date.now();
  const res = client.invoke('fintech.ledger.v1.LedgerService/Transfer', req, {
    metadata: {
      'x-tenant-id':       tenant,
      'x-actor-subject':   'k6',
      'x-actor-session':   `vu-${exec.vu.idInTest}`,
    },
    timeout: '1s',
  });
  const dt = Date.now() - t0;

  const ok = res && res.status === grpc.StatusOK;
  if (ok) {
    transferOK.add(1);
    transferLatency.add(dt, { kind: 'ok' });
  } else {
    transferErr.add(1, { code: res ? String(res.status) : 'no_response' });
    transferLatency.add(dt, { kind: 'err' });
  }

  check(res, { 'gRPC OK': (r) => r && r.status === grpc.StatusOK });
}

export function teardown() {
  client.close();
}

// ---- Helpers ---------------------------------------------------------------

function randInt(n) {
  return Math.floor(Math.random() * n);
}

// 80/20 Pareto skew: 80% of picks land in the first 1% of accounts,
// 20% spread uniformly. Approximates marketplace-operator distribution.
function paretoIdx(n) {
  if (Math.random() < 0.8) {
    const hot = Math.max(1, Math.floor(n / 100));
    return Math.floor(Math.random() * hot);
  }
  return Math.floor(Math.random() * n);
}
