# ledgerd

A double-entry payments ledger that runs as an HTTP service. Every movement
of money is a balanced journal entry that is never edited, and a crash at
any moment loses nothing that was acknowledged.

Written in Go with the standard library only (no third-party dependencies);
runs on Linux and macOS.

```console
$ curl -s localhost:8080/v1/transfers -H "$AUTH" -H 'Content-Type: application/json' \
    -H 'Idempotency-Key: order-1042' -d '{
      "currency": "USD", "pending": true, "timeout_seconds": 604800, "reference": "order-1042",
      "postings": [
        {"account": "alice", "direction": "debit",  "amount": 5000},
        {"account": "shop",  "direction": "credit", "amount": 4850},
        {"account": "fees",  "direction": "credit", "amount": 150}]}'
{"id":"tr_2f6c...","state":"pending","currency":"USD","postings":[...],"expires_at":"...","entries":[{"seq":7,"kind":"pending"}]}

$ curl -s localhost:8080/v1/accounts/alice -H "$AUTH"
{"id":"alice","currency":"USD","exponent":2,"normal_balance":"credit","non_negative":true,"version":2,
 "balance":{"debits_posted":0,"credits_posted":10000,"debits_pending":5000,"credits_pending":0,
            "posted":10000,"pending":-5000,"available":5000},"as_of_seq":7, ...}
```

## Accounts and transfers

Accounts have an ISO 4217 currency, a normal balance side (debit for assets
like cash at the bank, credit for wallets and revenue) and optional limits:
`non_negative` for a wallet that can't be overdrawn, `non_positive` for its
mirror. Each account reports a posted, pending and available balance, now or
as of any point in the journal, plus a paged statement with running balances.

A transfer has 2 to 64 postings in one currency, debits must equal credits,
and either every posting applies or none does. Amounts are `int64` minor
units and every addition is overflow-checked; there is no floating point
anywhere. A transfer can be pending: it reserves funds until it is posted
(all of it or part, as with a card capture), voided, or times out. Nothing
in the journal is ever edited. A refund is a reversal entry linked to the
original, and you can reverse part of a transfer as many times as there is
something left to send back.

## Retries, crashes and audits

Retries are safe. An `Idempotency-Key` gets you Stripe's behaviour: the same
request returns the original result, errors included; a different request
under the same key is refused; a duplicate that arrives mid-flight waits for
the first. The transfer and its key are written in the same log record, so
neither can survive a crash without the other. For read-then-write logic, a
transfer can require that an account's version or balance hasn't changed.

Storage is a checksummed command log, fsynced before anything is
acknowledged. Concurrent transfers share one fsync (group commit), a torn
tail is dropped on recovery, and the tests cut the log at every byte to
prove it. If the disk returns an error the service stops taking writes
rather than guess.

Reconciliation takes a bank or processor statement as CSV and reports what
matched, what matched by reference but with a different amount, and what is
missing on either side. An invariant checker (debits equal credits per
currency, every balance equals the fold of its entries, limits hold,
reservations match open pending transfers) runs at startup and on demand.

The HTTP layer checks API keys in constant time, rejects loose JSON, bounds
every size and wait, and emits JSON logs with request IDs and Prometheus
metrics.

## Getting started

```console
$ export LEDGERD_API_KEYS=local-dev-key-0123456
$ LEDGERD_DATA_DIR=./data go run ./cmd/ledgerd    # omit LEDGERD_DATA_DIR to keep everything in memory
$ ./examples/demo.sh                              # card payment, refund, retry, overdraft, reconciliation
```

Or with Docker (data in a volume; set an admin token, since requests from the
host do not arrive on the container's loopback):

```console
$ docker build -t ledgerd .
$ docker run -p 8080:8080 -v ledgerd-data:/data -e LEDGERD_API_KEYS=local-dev-key-0123456 \
    -e LEDGERD_ADMIN_TOKEN=local-admin-token-0123 ledgerd
$ LEDGERD_ADMIN_TOKEN=local-admin-token-0123 ./examples/demo.sh
```

Configuration is environment variables, listed in
[cmd/ledgerd/main.go](cmd/ledgerd/main.go).

## Endpoints

All `/v1` endpoints take `Authorization: Bearer <key>`. Write endpoints take
JSON bodies and an optional `Idempotency-Key` header.

| Endpoint | Purpose |
|---|---|
| `POST /v1/accounts` | Open an account. |
| `GET /v1/accounts/{id}` | Account and balance; `?as_of_seq=N` for the balance after entry N. |
| `GET /v1/accounts/{id}/statement` | Entries touching the account with running balances; `?after_seq=&limit=`. |
| `POST /v1/accounts/{id}/reconcile` | Match a `text/csv` statement; `?window_days=&from=&to=`. Read only. |
| `POST /v1/transfers` | Create a transfer; `"pending": true` for two-phase. |
| `GET /v1/transfers/{id}` | A transfer with its postings, state, links and entries. |
| `POST /v1/transfers/{id}/post` | Post a pending transfer: all, `{"amount":n}`, or per-posting amounts. |
| `POST /v1/transfers/{id}/void` | Release a pending transfer. |
| `POST /v1/transfers/{id}/reverse` | Reverse a posted transfer, fully or partly, as a new linked transfer. |
| `GET /v1/admin/invariants` | Run the invariant checker. Admin token, or loopback without one. |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | Liveness, readiness (fails after a storage error), Prometheus metrics. |

Errors look like `{"error":{"code":"limit_exceeded","message":"...","request_id":"..."}}`
with stable codes: `invalid_argument` 400, `unauthenticated` 401,
`permission_denied` 403, `not_found` 404, `already_exists` / `invalid_state` /
`transfer_expired` / `idempotency_in_progress` 409, `failed_precondition` 412,
`currency_mismatch` / `limit_exceeded` / `overflow` / `idempotency_key_reused` 422,
`resource_exhausted` 429, `unavailable` 503, `deadline_exceeded` 504.

## Verification

| What | How |
|---|---|
| Ledger logic | Random concurrent workloads (transfers, two-phase, partial posts, voids, expiries, reversals, idempotent retries, key reuse, crash-restarts) run against the real ledger; a separate single-threaded model with `big.Int` balances replays the writer's order from the log. Every result, balance, version and invariant must agree after every round. Deliberately broken versions of the ledger fail this test. |
| Crash safety | The log is truncated at every byte offset; recovery must produce exactly the state after some prefix of acknowledged commands, and every invariant must hold. Corruption in the middle of the log stops startup. |
| Storage failures | Injected write and fsync errors: the ledger stops accepting writes, keeps serving reads, and after a restart a keyed retry reports what actually reached the disk. |
| Idempotency | Replays across restarts, errors replayed, key reuse, scope per API key, retention expiry, concurrent duplicates held in flight by a blocked fsync. |
| Reconciliation | The amount-and-date pass is compared with a maximum bipartite matching on random inputs; a worst case of 10,000 lines against 100,000 identical movements must stay fast. |
| Parsers | Go fuzzing of the decimal amount parser, the log reader, the CSV parser and the transfer endpoints: accepted input round-trips; nothing returns a 500 or breaks an invariant. |
| API | Every endpoint and error code over real HTTP, a full card-payment flow, auth and admin rules, body limits, timeouts and request IDs. |
| Concurrency | The whole suite runs under the race detector in CI. |

To run the suite and the ledger benchmarks:

```console
$ go test -race ./...                 # full suite
$ go test -run=^$ -bench=. ./internal/ledger/
```

## Numbers

Apple M4 Max, single-phase transfers, every acknowledged transfer fsynced
(`F_FULLFSYNC`, about 4 ms per flush):

| Concurrent clients | fsync per transfer | Group commit | Transfers per fsync |
|---|---|---|---|
| 1 | 248 /s | 245 /s | 1 |
| 16 | 244 /s | 1,984 /s | 8 |
| 64 | 249 /s | 7,954 /s | 32 |
| 256 | 247 /s | 39,687 /s | 165 |

With one client there is nothing to batch, so both modes are bound by the
flush. The gain grows with the number of transfers that queue up while a
flush is in progress. Without the disk, the writer and state machine run
about 390,000 transfers/s, and a balance-as-of read takes 0.11 µs.

## Further reading

The design notes in [docs/DESIGN.md](docs/DESIGN.md) cover the data model,
the writer and group commit, the log format and recovery, idempotency,
reconciliation, the invariants, the reference-model test, limits, security
and what would change to run this across many machines. The code is laid
out as follows:

```text
cmd/ledgerd        server entry point and configuration
internal/money     currency table, checked arithmetic, exact decimal parsing
internal/wal       checksummed append-only log: batched append, replay, compaction
internal/ledger    accounts, transfers, journal, writer with group commit, idempotency, queries, invariants
internal/recon     statement CSV parser and matcher
internal/api       HTTP API, auth, errors, logging, metrics wiring
internal/metrics   Prometheus text exposition
examples/demo.sh   walkthrough against a running server
```

## License

MIT
