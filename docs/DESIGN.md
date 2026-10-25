# ledgerd design

ledgerd is built so that money is never created or lost: every change is a
balanced journal entry, every balance is the fold of its entries, and the
service checks both at startup and whenever an operator asks. A client that
times out can retry and get the original answer, including the original
error, without the payment happening twice. A response means the change is
on disk, and a crash at any byte loses only work that was never
acknowledged. Throughput should not collapse to the disk's flush rate, so
concurrent writes share each flush. It ships as one binary, uses the
standard library only, and has safe defaults.

This version does not do replication, ledgers larger than memory, or
multi-currency entries. They are discussed under
[Past a single writer](#past-a-single-writer) and
[Accounts, entries and transfers](#accounts-entries-and-transfers).

## Accounts, entries and transfers

The model follows double-entry bookkeeping (Pacioli's *Summa*; Modern
Treasury's [Accounting for Developers](https://www.moderntreasury.com/journal/accounting-for-developers-part-i)
is a good modern account) and TigerBeetle's
[account and transfer model](https://docs.tigerbeetle.com/).

An account has an ID, an ISO 4217 currency (the minor-unit exponent comes from a
built-in table of active codes; codes without a minor unit such as XAU are
refused), a *normal balance* side, optional limit flags and bounded metadata.
An account keeps four running totals, all `int64` minor units:

```text
debits_posted   credits_posted   debits_pending   credits_pending
```

Clients see balances signed in the account's normal sense. For a
credit-normal account (a customer wallet, a liability on our books):

```text
posted    = credits_posted - debits_posted
pending   = credits_pending - debits_pending
available = credits_posted - debits_posted - debits_pending
```

Debit-normal accounts (cash at bank, an asset) swap the sides. *Available*
counts reservations that would lower the balance but not those that would
raise it: money is spendable once it arrives, not when it is promised.

Of the limit flags, `non_negative` refuses any entry that would take
`available` below zero (TigerBeetle's `debits_must_not_exceed_credits` on a
credit-normal account). `non_positive` refuses any entry that would take
`posted + incoming pending` above zero. A change that moves the balance
*towards* the limit is always allowed, so posting, voiding or expiring a
reservation can never be blocked by a limit.

A transfer is the business object a client
creates; journal entries are the immutable records of what happened to it.

| Operation | Entry kind | Effect per posting |
|---|---|---|
| single-phase transfer | `posted` | posted += amount |
| pending transfer | `pending` | pending += amount |
| post (capture) | `post_pending` | pending -= reserved, posted += captured |
| void | `void_pending` | pending -= reserved |
| timeout | `expire_pending` | pending -= reserved |
| reversal | `reversal` | posted += amount on the opposite side |

A transfer has 2 to 64 postings, each a debit or credit to a distinct
account, all in the transfer's one currency. Debits must equal credits or the
transfer is refused. One currency per entry is a deliberate constraint: an FX
conversion is two entries through per-currency FX accounts. Allowing
multi-currency entries balanced per currency would make the conversion atomic
and is a contained change to `post`, but it also needs FX-rate provenance in
the entry, which is out of scope.

Money is `int64` minor units, never floating point. Every addition is
overflow-checked; in addition, posted plus pending on each side of an account
must fit in an `int64`, which guarantees the subtractions above cannot
overflow. A transfer that would break this is refused with `overflow`. Sums
across accounts (for the invariant checker) use a 128-bit accumulator.

Two-phase transfers follow TigerBeetle's
[two-phase transfers](https://docs.tigerbeetle.com/coding/two-phase-transfers/):
a pending transfer reserves its amounts against the limits; posting it can
capture less than the reservation (per posting, as long as the capture still
balances), and the rest is released; voiding releases everything. A pending
transfer has a timeout (default 24 h, at most 30 days). Posting is one-shot:
after a partial capture the remainder is gone, as with card authorizations.

History is immutable: there is no update or delete. A correction is a
reversal: a new transfer with the original postings' sides flipped, linked
both ways (`reverses` / `reversals`). Reversals may be partial and repeated
until the posted amount is used up; each posting tracks `reversed`. A
reversal is an ordinary entry, so limits apply: refunding a merchant that has
already paid out its balance is refused, not silently overdrawn. A reversal
cannot itself be reversed; post a new transfer instead.

## One writer, shared fsyncs

All changes go through one writer goroutine, the way TigerBeetle runs a
single-threaded state machine. A request is validated (everything that does
not depend on state), queued, and then:

1. The writer takes every request that is waiting, up to `MaxBatch` (1024).
2. It expires due reservations, then applies each command in order to a
   *transaction* layered over the committed state. Each command checks
   everything before it changes anything, so a refused command leaves no
   trace and later commands in the batch see exactly the accepted ones.
3. It writes the log records for the whole batch with one write and one
   fsync.
4. Only then does it take the state lock briefly, publish the transaction,
   and reply.

Readers take a shared lock on the committed state and never see a change
that is not yet durable. While one batch is being flushed, the next one
queues up behind it; that queueing is the whole group-commit mechanism
(DeWitt et al., *Implementation Techniques for Main Memory Database
Systems*). Batch size adapts to load with no timer: an idle server flushes a
single transfer immediately.

On an Apple M4 Max, where Go's `fsync` is `F_FULLFSYNC` (about 4 ms), one
fsync per transfer caps out near 250 transfers/s regardless of concurrency.
With group commit the same disk does 2,000 transfers/s at 16 clients and
40,000 at 256 (`BenchmarkGroupCommit`); the number of transfers per fsync
tracks the number of waiting clients.

The cost of a single writer is that write throughput is bounded by one core. The state
machine costs about 3 µs per transfer in memory, far below the disk's cost, so
the disk remains the bottleneck. Partitioning is the answer beyond that (see
[Past a single writer](#past-a-single-writer)).

## The log, and recovering from it

The log is a command log. Each line is:

```text
<crc32c of payload, 8 lowercase hex digits> <JSON record>\n
```

Records are `header` (format version), `cmd` (an accepted command with the
time it ran, the ID it generated and the entry sequence it produced),
`expire` (a timeout released a reservation) and `reject` (a refused command
that carried an idempotency key, with its error). Replay runs each `cmd`
through the same code at the recorded time, so the rebuilt state is
identical, and it checks that each record produces the sequence number the
log says it did. Expiries are records of their own, so recovery never
consults the clock.

On start the log file is locked with an exclusive `flock`, so a second
process pointed at the same directory refuses to start instead of
interleaving appends; the kernel drops the lock when the process dies, even
on `kill -9`. Then:

- A torn final line (no newline, or a bad checksum on the last line) belongs
  to a batch that was never acknowledged; it is dropped and the file
  truncated.
- Any other bad line, or a record that fails to replay, means the file was
  damaged after it was acknowledged. Startup stops with an error rather than
  guess which acknowledged transfers to throw away.
- The log is compacted: rewritten through a temporary file, fsynced, and
  atomically renamed over the old one, dropping `reject` records and
  idempotency keys older than the retention window. Entries are history and
  are always kept, so the compacted log has the same structure as the
  original and recovery from it is the same code.
- Every invariant (below) is checked before the server accepts traffic.

The crash test writes a workload of accounts, single-phase and two-phase
transfers, captures, reversals, an expiry and a keyed rejection, recording
the state after each acknowledged command. It then truncates the log at
every byte offset and recovers each prefix: the result must equal the
state after some acknowledged prefix, never go backwards as the offset grows,
and satisfy every invariant.

Storage errors are fail-stop. If a write or fsync fails, the ledger stops
accepting writes (HTTP 503, `/readyz` fails) and keeps serving reads of the
last durable state. It does not retry the fsync: after a failed fsync the
kernel may already have dropped the dirty pages, so neither the file nor a
retry can be trusted ([PostgreSQL's fsync() surprise](https://lwn.net/Articles/752063/);
Rebello et al., *Can Applications Recover from fsync Failures?*). A restart
recovers from what is really on disk, and clients learn the outcome of the
ambiguous requests by retrying with their idempotency keys. The tests inject
both failures and check exactly that.

## Retrying with an idempotency key

The semantics are Stripe's
([idempotent requests](https://docs.stripe.com/api/idempotent_requests),
[Designing robust and predictable APIs with idempotency](https://stripe.com/blog/idempotency)):

- The first request with an `Idempotency-Key` executes. Its result, success
  or error, is stored and returned to every later request with the same key
  and the same parameters, with `Idempotent-Replayed: true` and the original
  status code. An overdraft stays an overdraft on retry even after the account
  is funded, which is what makes retries safe.
- The same key with different parameters is refused with
  `idempotency_key_reused` (422). Parameters are compared as a SHA-256 of the
  method, path and the decoded body re-encoded canonically, so whitespace and
  field order don't matter.
- A request arriving while the first is still executing waits for it, and
  gets `idempotency_in_progress` (409) if its own deadline passes first.
- Requests that fail validation are not stored (the client can fix the
  request and reuse the key), and nor are requests the ledger never executed
  (canceled before the writer reached them, or storage unavailable).
- Keys are scoped per API key, so two clients can neither collide nor read
  each other's results. Rotating an API key therefore starts a fresh key space.
- Keys are kept for `LEDGERD_IDEMPOTENCY_TTL` (default 24 h, like Stripe).

What makes this safe is that the effect and the key are one log record,
fsynced together. There is no window where a transfer is durable but its key
is not (a retry would double-charge) or the reverse (a retry would return a
result that does not exist).

## Preconditions instead of locks

Each account has a `version`, the number of entries that have touched it. A
transfer may carry preconditions `{account, version}` and/or
`{account, posted_balance}`; if any does not hold when the writer reaches the
transfer, it is refused with `failed_precondition` (412). This is the usual
read-decide-write pattern (Modern Treasury exposes the same idea as a ledger
account lock version) without holding locks across a client round trip.

## Reading balances and statements

Each account keeps its history as `(sequence, line, balance after)`, so a
balance as of entry *n* is a binary search over that account's rows, and a
statement is the same rows paged by `after_seq`, each with its signed posted
and pending change and the running balance after it. Committed transfers are
immutable objects (a change publishes a new copy), so `GET /v1/transfers/{id}`
hands out a pointer without copying or holding a lock while it encodes.

## Matching a bank statement

`POST /v1/accounts/{id}/reconcile` takes a CSV statement
(`external_id,date,amount,currency,reference`) and matches it against the
account's posted movements. Amounts are decimals in major units, signed so a
positive amount raises the account's normal balance, parsed exactly with the
currency's exponent (more decimals than the currency has is an error, never a
rounding). The parser is bounded (4 MiB, 10,000 lines, field lengths) and
fuzzed: anything it accepts must survive a format-and-parse round trip.

Matching runs in two passes:

1. By reference: a line whose reference equals a movement's reference is the
   same transaction. An exact amount is preferred; otherwise it is reported as
   an *amount mismatch* rather than as two unrelated breaks.
2. By amount and date: remaining lines, in date order, take the earliest free
   movement with the same amount whose date is within `window_days` (default
   3, for settlement lag) before or after. For this interval structure the
   greedy choice matches as many lines as any assignment can; a test compares
   it with a maximum bipartite matching on random inputs. A union-find "next
   free" index per amount keeps the pass near-linear when thousands of
   movements share an amount.

What is left is *missing in ledger* (statement lines) or *missing in
statement* (movements dated inside the period). Movements up to the window
outside the period are candidates for matching but are not reported as
missing. The report never changes the ledger; fixing a break is a transfer or
a reversal. Dates are UTC.

## What the invariant checker proves

`GET /v1/admin/invariants` and startup both run the checker:

1. every journal entry balances, posted and pending;
2. per currency, total debits equal total credits across all accounts;
3. every account's totals equal the fold of the entries that touch it, and
   its running balances and version agree with that fold;
4. no limit-flagged account is past its limit;
5. every account's pending totals equal the open pending transfers' postings
   to it, and the count of open transfers matches;
6. every transfer's posted and reversed amounts agree with its postings and
   its reversals.

The checker reads under the shared lock and is linear in the journal size,
so on a large ledger it delays commits while it runs. It is an operator tool;
at scale it runs against a replica (see
[Past a single writer](#past-a-single-writer)).

## Checking the ledger against an independent model

`TestMatchesReferenceModel` runs random workloads against the real ledger:
concurrent batches of single-phase and pending transfers (with overflow-sized
amounts, wrong currencies, unknown accounts and stale preconditions mixed
in), posts, partial posts, voids, reversals, clock advances that expire
reservations, idempotent retries (including concurrent duplicates), key reuse
with different requests, and crash-restarts from a copy of the log with a
torn half-batch appended.

The order the writer chose is read back from the log (every operation carries
an idempotency key, so even refusals are recorded), and a separate
single-threaded model replays the same operations in that order. The model
keeps balances as `big.Int` and shares no code with the ledger. After every
round, every result code, every returned transfer, every balance and version,
and every invariant must agree. The test also asserts that the workload
reached every outcome (each error code, each operation, expiries, replays,
crashes), so a passing run cannot be a vacuous one.

## Limits

| Limit | Value | Why |
|---|---|---|
| Postings per transfer | 64 | Bounds the work and record size of one command. |
| Preconditions per transfer | 64 | Same. |
| Metadata | 20 keys, 40-char keys, 500-char values | Stripe's per-key limits, fewer keys. |
| IDs | 64 chars, `[A-Za-z0-9][A-Za-z0-9_.:-]*` | Safe in paths and logs. |
| Reference | 128 chars | |
| Idempotency key | 255 printable ASCII chars | Stripe's limit. |
| Idempotency retention | 24 h (1 min to 30 days) | Memory is proportional to keys in the window. |
| Pending timeout | 1 s to 30 days, default 24 h | |
| JSON body | 1 MiB | Checked before decoding. |
| Statement CSV | 4 MiB, 10,000 lines | |
| Reconciliation | 4 at once, period 366 days, window 31 days, 100,000 movements | Bounds memory per request. |
| Statement page | 1,000 lines | |
| Log record | 1 MiB | The largest valid command encodes to well under this (tested). |
| Batch | 1,024 commands per fsync | |
| Request deadline | 5 s | A timed-out write may still commit; retry with the key to find out. |

## Hardening

- API keys (at least 16 characters) are compared as SHA-256 digests with
  `subtle.ConstantTimeCompare` against every configured key, so timing reveals
  neither which key nor which prefix matched.
- Admin endpoints need `LEDGERD_ADMIN_TOKEN` (which must differ from every API
  key). Without one they answer only loopback clients that present an API
  key. Behind a proxy or in a container, set the token.
- Strict JSON: unknown fields, trailing data, floats and strings for amounts
  are refused; bodies are capped before they are read.
- Errors from storage or bugs return a generic message and the request ID;
  details go only to the server log. Secrets and request bodies are never
  logged.
- Client request IDs are kept only if they match a safe pattern.
- The log and its directory are created with modes 0600 and 0700. The
  container runs as a non-root user on a distroless image.

## Past a single writer

To survive the loss of a machine, the batch append goes behind Raft (or a
managed log), so a batch is committed on a quorum before it is
acknowledged. The writer already produces exactly one append per batch,
which is the unit a consensus round would carry. Followers replay the same
command log and serve reads, statements and the invariant checker without
touching the leader.

Beyond one writer's throughput, ledgers are sharded by account. Transfers
within a shard stay single-writer. Cross-shard transfers become two-phase
across shards: a pending transfer on each side, posted together, which this
model already supports, coordinated by a small transfer manager with its own
log.

History that no longer fits in memory would be split: balances and open
transfers stay in memory, and old entries move to an LSM or columnar store
keyed by `(account, sequence)`. Statements and as-of reads then read cold
history, and compaction can write balance checkpoints so recovery starts
from the last checkpoint instead of the first entry.

Idempotency keys are already part of the log. A sharded deployment keeps
them with the shard that executed the command and routes retries by key.
