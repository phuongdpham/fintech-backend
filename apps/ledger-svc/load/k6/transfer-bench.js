// transfer-bench.js — focused A/B benchmark scenario.
//
// Constant-arrival-rate at $RPS for $DURATION. No ramp: we want a flat
// load profile so before/after numbers are directly comparable. Runs
// the same per-iteration logic as transfer.js (Pareto skew, identity
// headers, same idempotency-key shape) so the only variable when
// diffing two runs is the server-side code.
//
// Run:
//   k6 run apps/ledger-svc/load/k6/transfer-bench.js \
//       -e RPS=4000 -e DURATION=4m \
//       -e FIXTURES=apps/ledger-svc/load/fixtures.json \
//       -e ADDR=localhost:9090 \
//       -e PROTO_DIR=shared/proto

import grpc from 'k6/net/grpc';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import exec from 'k6/execution';

const FIXTURES_PATH = __ENV.FIXTURES || 'apps/ledger-svc/load/fixtures.json';
const ADDR          = __ENV.ADDR     || 'localhost:9090';
const PROTO_DIR     = __ENV.PROTO_DIR || 'shared/proto';
const PROTO_FILE    = 'fintech/ledger/v1/ledger.proto';

const fixtures = JSON.parse(open(`../../../../${FIXTURES_PATH}`));

const client = new grpc.Client();
client.load([`../../../../${PROTO_DIR}`], PROTO_FILE);

const transferLatency = new Trend('transfer_latency_ms', true);
const transferOK      = new Counter('transfer_ok_total');
const transferErr     = new Counter('transfer_err_total');

const RPS      = parseInt(__ENV.RPS || '4000', 10);
const DURATION = __ENV.DURATION || '4m';

// VU sizing rule of thumb: VUs ≥ RPS × p99_seconds × 1.5. At RPS=4000
// and p99~50ms expected, that's ~300. preAllocate generously so k6
// doesn't burn cycles spawning under load.
export const options = {
  scenarios: {
    transfer_const: {
      executor: 'constant-arrival-rate',
      rate: RPS,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(300, Math.floor(RPS * 0.1)),
      maxVUs: Math.max(600, Math.floor(RPS * 0.2)),
    },
  },
  thresholds: {
    'transfer_latency_ms{kind:ok}': ['p(99)<200'],
  },
};

export function setup() {
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

  const idemKey = `bench-${exec.scenario.iterationInTest}-${exec.vu.idInTest}`;
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
      'x-tenant-id':      tenant,
      'x-actor-subject':  'bench',
      'x-actor-session':  'bench-session',
    },
    timeout: '1s',
  });
  const elapsed = Date.now() - t0;

  const ok = res && res.status === grpc.StatusOK;
  transferLatency.add(elapsed, { kind: ok ? 'ok' : 'err' });
  if (ok) {
    transferOK.add(1);
  } else {
    transferErr.add(1);
  }
  check(res, { 'ok': (r) => r && r.status === grpc.StatusOK });
}

// Pareto skew: 80% of writes target the first 1% of accounts. Same
// shape as transfer.js so any comparison stays apples-to-apples.
function paretoIdx(n) {
  const u = Math.random();
  if (u < 0.8) {
    return Math.floor(Math.random() * Math.max(1, Math.floor(n * 0.01)));
  }
  return Math.floor(Math.random() * n);
}

function randInt(n) {
  return Math.floor(Math.random() * n);
}
