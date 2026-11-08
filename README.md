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
mirror. Those are the opening values of two balance limits that can be
changed at any time as amounts: an overdraft limit (how far below zero the
available balance may go) and a credit limit (how high the balance may go),
each logged with the journal sequence it takes effect at and kept in a
history. Each account reports a posted, pending and available balance, now or
as of any point in the journal, plus a paged statement with running balances.
An account can be frozen, which stops money moving through it while its
existing holds can still be voided or expire, unfrozen again, or closed once
its balance is zero and it holds no reservations; a closed account takes no
more entries.

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
acknowledged. The log is split into fixed-size segments; a full one is
sealed with a checksummed footer, and segments that only hold records
already in a kept snapshot are deleted. Concurrent transfers share one fsync (group commit), a torn
tail is dropped on recovery, and the tests cut the log at every byte to
prove it. If the disk returns an error the service stops taking writes
rather than guess. Periodic checksummed snapshots let a restart load the
state and replay only the log written since: a million-entry ledger opens in
2.5 s instead of 9.4 s.

Reconciliation takes a bank or processor statement as CSV and reports what
matched, what matched by reference but with a different amount, and what is
missing on either side. An invariant checker (debits equal credits per
currency, every balance equals the fold of its entries, limits hold,
reservations match open pending transfers) runs at startup and on demand;
it checks a frozen copy-on-write view of the ledger, so commits keep going
while it scans.
`ledgerd verify` checks a stopped server's data directory offline: every
checksum in every segment and snapshot, segment continuity, every invariant,
and that replaying the log reproduces each snapshot exactly. It exits 0 for
sound, 1 for damaged and 2 for repairable (a normal start fixes it without
losing an acknowledged write).

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

To check a data directory while the server is stopped (a backup or a copy works too):

```console
$ go run ./cmd/ledgerd verify ./data          # or -json for the full report
```

To move or restore a server, copy its whole data directory while no server
has it open (after a stop, or after the process or machine died), run
`ledgerd verify` on the copy (0 or 2 means every acknowledged write is
there) and start `ledgerd` on it. Clients then retry unanswered requests
with the same `Idempotency-Key`. A running server's directory is not a
consistent copy. The file formats are documented in
[docs/DESIGN.md](docs/DESIGN.md#on-disk-formats).

Configuration is environment variables, listed in
[cmd/ledgerd/main.go](cmd/ledgerd/main.go).

## Endpoints

All `/v1` endpoints take `Authorization: Bearer <key>`. Write endpoints take
JSON bodies and an optional `Idempotency-Key` header.

| Endpoint | Purpose |
|---|---|
| `POST /v1/accounts` | Open an account. |
| `GET /v1/accounts` | List accounts in ID order with balances; `?currency=&metadata[key]=value&limit=&cursor=`. Pages carry `has_more` and an opaque, signed `next_cursor`. |
| `GET /v1/accounts/{id}` | Account, balance, `status` (`open`, `frozen`, `closed`, with `status_reason` and `status_changed_at`) and `limits` (`overdraft`, `credit`, null where none); `?as_of_seq=N` for all of them as of entry N. |
| `GET /v1/accounts/{id}/statement` | Entries touching the account with running balances; `?after_seq=&limit=`. |
| `POST /v1/accounts/{id}/limits` | Set one balance limit: `{"kind":"overdraft"\|"credit","amount":N}`, or `"unlimited":true` to remove it; optional reason. Takes effect from the next journal entry (`limits.effective_seq`); an account already past a lowered limit can only move back towards it. |
| `GET /v1/accounts/{id}/limits` | The limits in force and every change made to them, oldest first. |
| `POST /v1/accounts/{id}/freeze` | Freeze an open account; optional `{"reason":"..."}`. New transfers, posts of its holds and reversals that touch it get `account_frozen`; voids and expiries still apply. |
| `POST /v1/accounts/{id}/unfreeze` | Return a frozen account to open; optional reason. |
| `POST /v1/accounts/{id}/close` | Close an account whose posted balance is zero and that holds no reservations (else `failed_precondition`); optional reason. Final: anything touching it afterwards gets `account_closed`. |
| `POST /v1/accounts/{id}/reconcile` | Match a `text/csv` statement; `?window_days=&from=&to=`. Read only. |
| `POST /v1/transfers` | Create a transfer; `"pending": true` for two-phase. |
| `GET /v1/transfers` | List transfers in creation order, `order=desc` for newest first; `?account=&state=&currency=&reference=&metadata[key]=value&limit=&cursor=`. `reference=` finds the transfers that carry an external reference. Same cursors as accounts. |
| `GET /v1/transfers/{id}` | A transfer with its postings, state, links and entries. |
| `POST /v1/transfers/{id}/post` | Post a pending transfer: all, `{"amount":n}`, or per-posting amounts. |
| `POST /v1/transfers/{id}/void` | Release a pending transfer. |
| `POST /v1/transfers/{id}/reverse` | Reverse a posted transfer, fully or partly, as a new linked transfer. |
| `GET /v1/admin/invariants` | Run the invariant checker. Admin token, or loopback without one. |
| `GET /healthz`, `GET /readyz`, `GET /metrics` | Liveness, readiness (fails after a storage error), Prometheus metrics. |

Errors look like `{"error":{"code":"limit_exceeded","message":"...","request_id":"..."}}`
with stable codes: `invalid_argument` 400, `unauthenticated` 401,
`permission_denied` 403, `not_found` 404, `already_exists` / `invalid_state` /
`transfer_expired` / `idempotency_in_progress` / `account_frozen` /
`account_closed` 409, `failed_precondition` 412,
`currency_mismatch` / `limit_exceeded` / `overflow` / `idempotency_key_reused` 422,
`resource_exhausted` 429, `unavailable` 503, `deadline_exceeded` 504.

## Verification

| What | How |
|---|---|
| Ledger logic | Random concurrent workloads (transfers, two-phase, partial posts, voids, expiries, reversals, freezes, unfreezes, closes, limit changes, idempotent retries, key reuse, crash-restarts) run against the real ledger; a separate single-threaded model with `big.Int` balances replays the writer's order from the log. Every result, balance, version, status, limit, point-in-time read, invariant and listing walk (every filter, both orders, changing page sizes) must agree after every round. Deliberately broken versions of the ledger and of its indexes fail this test. |
| Crash safety | The log is truncated at every byte offset, including across segment boundaries and seals, with and without snapshots beside it (and with the newest snapshot never renamed); recovery must produce exactly the state after some prefix of acknowledged commands, the same state with or without snapshots, and every invariant must hold. Corruption in the middle of the log stops startup. |
| Power loss | The log and snapshots run on an in-memory file system that, on a simulated power cut, keeps what was synced plus a random selection of unsynced writes and directory changes: dropped, torn, reordered. The log survives thousands of cuts inside Open, appends, rolls, drops and legacy adoption with every acknowledged record intact; the reference-model workload survives thousands more mid-command, mid-snapshot and mid-drop, with every acknowledged command and result intact and the model agreeing on every balance. |
| Snapshots | A snapshot plus the log after it must rebuild exactly the state (accounts, history rows, transfers, journal, expiries, idempotency results) that replaying the whole log does, across random workloads, crashes mid-snapshot, concurrent load and dropped log segments. A snapshot cut at every byte or with a flipped bit in any byte is refused, and recovery falls back to an older snapshot or the log. |
| Segments | Every byte of a sealed segment is cut and bit-flipped and must be refused; a missing, unlisted or out-of-order segment and a damaged manifest stop startup; a crash at each step of sealing, dropping and adopting an old single-file log reopens to exactly the acknowledged records. The reference-model test runs with segments of 256 bytes to 4 KiB, so it rolls and drops segments and crashes across them. |
| Kill and restore | The real binary is loaded by 8 concurrent clients and killed with `SIGKILL` three times, each time once it has written a snapshot and dropped segments. Its directory is copied, `ledgerd verify` must pass on the copy, and the next server runs on the copy: every acknowledged request resent with its key must replay its original answer, every balance must equal the acknowledged history, and after a clean stop the copy verifies as sound and restarts to the same balances. |
| Offline verify | `ledgerd verify` is run on sound ledgers (with and without dropped segments), on each kind of crash debris the server repairs, and on each kind of damage it refuses: flipped bytes, missing segments, lost snapshots, a snapshot rewritten with valid checksums that disagrees with the log or breaks an invariant, and a newest snapshot the log does not mark. Each must get the right verdict, a repairable directory must then open to the same head, and verify must never change a file. |
| Invariant checker | Every kind of damage is reported, including damage only a copy saved for the check still holds. Transfers, reservations, captures, voids, reversals and new accounts committed at every point the checker pauses must leave it reporting the head it froze and no violation, and checks run back to back under a concurrent mixed workload must all be clean. On a large ledger, commit latency during a check must stay a small fraction of the check's duration (a commit that waited for the scan fails it). |
| Storage failures | Injected write and fsync errors, also at each step of a segment roll: the ledger stops accepting writes, keeps serving reads, and after a restart a keyed retry reports what actually reached the disk. A failure or power cut at any step of a snapshot write (including between the rename and the directory fsync) loses nothing and drops no segment early. |
| Idempotency | Replays across restarts, errors replayed, key reuse, scope per API key, retention expiry, concurrent duplicates held in flight by a blocked fsync. |
| Reconciliation | The amount-and-date pass is compared with a maximum bipartite matching on random inputs; a worst case of 10,000 lines against 100,000 identical movements must stay fast. |
| Parsers | Go fuzzing of the decimal amount parser, the log reader, the CSV parser, the transfer, freeze, close and limit endpoints, and the listing query strings with every cursor they return followed: accepted input round-trips; nothing returns a 500, breaks an invariant or an index, and no walk repeats an item or fails to end. |
| API | Every endpoint and error code over real HTTP, a full card-payment flow, auth and admin rules, body limits, timeouts and request IDs. |
| Concurrency | The whole suite runs under the race detector in CI. |

To run the suite and the ledger benchmarks:

```console
$ go test -race ./...                 # full suite
$ go test -run=^$ -bench=. ./internal/ledger/
$ go test -run=^$ -bench=Recovery -benchtime=3x ./internal/ledger/   # restart time at 1 million entries
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
reconciliation, the invariants, the reference-model test, limits, security,
the on-disk formats and what would change to run this across many machines.
Changes are listed in [CHANGELOG.md](CHANGELOG.md). The code is laid
out as follows:

```text
cmd/ledgerd        server entry point and configuration
internal/money     currency table, checked arithmetic, exact decimal parsing
internal/wal       checksummed append-only log in sealed segments: batched append, replay, segment drop
internal/ledger    accounts, transfers, journal, writer with group commit, idempotency, snapshots, queries, invariants
internal/recon     statement CSV parser and matcher
internal/api       HTTP API, auth, errors, logging, metrics wiring
internal/metrics   Prometheus text exposition
examples/demo.sh   walkthrough against a running server
```

## License

MIT
