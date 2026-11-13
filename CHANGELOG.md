# Changelog

All notable changes to ledgerd. Each entry names the roadmap issue it came
from. Versions follow [Semantic Versioning](https://semver.org/) once 1.0 is
released; until then the API and the on-disk formats may change between
releases, and [docs/DESIGN.md](docs/DESIGN.md#on-disk-formats) documents the
current formats.

## Unreleased

### Batches, balancing transfers, holds and multi-currency (#26)

#### Added

- Atomic batches: `POST /v1/transfers/batch` creates up to 8,190 linked
  transfers as one `create_batch` command, written as one log record with
  one fsync. The transfers run in order, each seeing the effects of the ones
  before it; if any is refused, none happens. The response has one result
  per transfer (`created`, or `failed` and `not_applied`); a refusal carries
  the failing transfer's status and error, with its index in `error.item`.
  One `Idempotency-Key` covers the whole batch, and its result, including a
  refusal, replays across restarts. A batch is rolled back out of a group
  commit it shares with other commands without affecting them.
- Balancing transfers: `balance_debit` and `balance_credit` on a
  two-posting transfer cut its amount to what the flagged account can take
  without crossing its limits, a limit that is not set counting as zero
  (TigerBeetle's rule), for posted transfers and holds alike. The transfer's
  `balancing` records `requested_amount` and the `amount` that moved; with
  nothing to move it is refused with `limit_exceeded`. The invariant checker
  verifies the record against the postings.
- Growing and extending holds: `POST /v1/transfers/{id}/increase`
  (`increase_pending`) reserves more on a pending transfer, by `amount` or
  balanced per-posting `postings`, checked against limits like a new hold;
  `POST /v1/transfers/{id}/extend` (`extend_pending`) moves its timeout to
  `timeout_seconds` from now, later only. Both are logged, keyed and listed
  in the transfer's `entries`; an extension's journal entry has no lines
  and carries the new `expires_at`. Balancing holds cannot grow, and
  neither change applies while one of the hold's accounts is frozen. The
  invariant checker ties each hold's amounts and timeout to its entries.
- Multi-currency transfers: a posting may name its own `currency`, so one
  transfer, one journal entry and one log record can convert between two
  currencies. It must balance in each currency and carry `fx` (`rate` as a
  plain decimal string, `source`, `quoted_at`), stored on the transfer and
  on the entry that creates it. Accounts opened with `"exchange": true` are
  exchange accounts; the rate is checked exactly (`math/big`) against what
  moves through them, rounded either way to a minor unit, so fees and
  spreads can be posted elsewhere in the entry. Explicit amounts for a
  partial post, an increase or a partial reversal of a multi-currency
  transfer are held to its rate; a reversal of both sides carries it.
  Transfers are listed under both their currencies, and the invariant
  checker verifies per-currency balance, rates and posting currencies.

#### Changed

- The log's record limit is 16 MiB, up from 1 MiB, so the largest batch
  fits in one record. Lines over 1 MiB are assembled in a buffer of their
  own, so the pooled replay readers stay at 1 MiB.
- Snapshot format 4 stores batch results and the failing item of a batch
  refusal for kept idempotency keys. Format 1 to 3 files still load.
- Snapshot format 5 stores each transfer's balancing record. Format 1 to 4
  files still load.
- Snapshot format 6 allows `increase_pending` and `extend_pending` entries
  and stores an extension's timeout. Format 1 to 5 files still load.
- Snapshot format 7 stores the exchange flag, posting and line currencies
  and rates. Format 1 to 6 files still load.
- Amounts given per posting are summed with overflow checks, returning
  `overflow` instead of relying on the transfer's own totals.
- Ledger errors carry `item` when they are about one transfer of a batch.

#### Tests

- The reference model covers atomic batches (with rollback and the refused
  item), balancing transfers, hold increases and extensions, and
  conversions between USD, EUR and JPY through exchange accounts, including
  partial posts, increases and reversals of them. It checks every returned
  transfer in full (currencies, rate, balancing record, timeout, entries,
  reversals) and, after every round, that debits equal credits in each
  currency, posted and pending. The power-cut and snapshot tests run the
  same workload.

### Listings, indexes and account lifecycle (#25)

#### Added

- Ordered secondary indexes of accounts (by currency and metadata pair) and
  transfers (by state, currency, account, reference and metadata pair),
  kept up to date on every commit and rebuilt when a snapshot loads, in
  about 0.28 s for 1 million entries at 47 bytes per transfer.
  `ledgerd verify` checks them against a full scan.
- `GET /v1/accounts`: accounts in ID order with their balances, filtered by
  `currency` and `metadata[key]=value` pairs, up to 1,000 per page. Pages
  are consistent as of one journal entry (`as_of_seq`) and stable while
  other clients write. Cursors are opaque, bound to their filters and
  signed with HMAC-SHA256 using `LEDGERD_CURSOR_SECRET`, or a key derived
  from the API keys when it is unset. Each request scans a bounded number
  of candidates, so rare filter combinations return short pages instead of
  holding the read lock.
- `GET /v1/transfers`: transfers in creation order, or newest first with
  `order=desc`, filtered by `account`, `state`, `currency`, `reference` and
  `metadata[key]=value` pairs, with the same signed cursors (bound to the
  order too), limits and scan budget as accounts. Filtering by `reference`
  is a direct lookup by external reference that reads only that
  reference's index set.
- Account freeze, unfreeze and close: `POST /v1/accounts/{id}/freeze`,
  `/unfreeze` and `/close`, logged as the new `freeze_account`,
  `unfreeze_account` and `close_account` commands, each with an optional
  reason. A frozen account refuses new transfers, posts of its holds and
  reversals with `account_frozen` (409), while its holds can still be
  voided or expire. Closing needs a zero posted balance and no open
  reservations (`failed_precondition` otherwise) and is final; a closed
  account refuses everything with `account_closed` (409). Accounts report
  `status`, `status_reason` and `status_changed_at`, also as of any journal
  entry, and the invariant checker proves no entry moved money through a
  frozen or closed account.
- Balance limits as amounts: an overdraft limit (how far below zero the
  available balance may go) and a credit limit (how high the balance
  including incoming holds may go). `POST /v1/accounts/{id}/limits` logs a
  new `set_limit` command that sets one of them or removes it
  (`"unlimited":true`), with an optional reason; it takes effect from the
  next journal entry, reported as `effective_seq`. `non_negative` and
  `non_positive` are now the opening limits of 0. Lowering a limit below
  what an account uses is allowed; the account can then only move back
  towards it. Accounts report `limits`, also as of any journal entry, and
  `GET /v1/accounts/{id}/limits` lists every change. The writer enforces
  the limit in force inside the batch, and the invariant checker checks
  every entry against the limit of its time.

- `examples/demo.sh` walks an account's lifecycle over HTTP after the card
  payment: open two tagged wallets and fund one, freeze it (a payout is
  refused with `account_frozen`), list the cohort one page at a time by
  following the signed cursor, look up a transfer by reference, unfreeze
  and raise the overdraft limit so a larger payout goes through, then close
  it (refused while it owes money, accepted at zero, and closed for good).
  `LEDGERD_DEMO_RUN` sets the suffix of its account IDs.

#### Changed

- A snapshot listing a transfer with no journal entries is refused.
- Snapshot format 2 stores each account's status history and the status in
  stored idempotent results. Format 1 snapshots still load, with every
  account open.
- Snapshot format 3 stores each account's limit history and the limits in
  stored idempotent results. Format 1 and 2 snapshots still load, with the
  limits the account flags give.
- The invariant checker's limit check (invariant 4) now judges each entry
  against the limit in force at its sequence instead of only the final
  balance against the flags.

#### Tests

- The reference model covers freeze, unfreeze, close and `set_limit`,
  accounts with metadata and transfers with references and metadata. After
  every round it checks every account's status, limits and limit history,
  a point-in-time read, and walks of both listings across every kind of
  filter, both orders and changing page sizes against its own answers.
  Eight broken-index variants must each be caught by those walks alone.
- `FuzzCreateTransfer` also posts its input to the freeze, unfreeze, close
  and limit endpoints and checks the indexes; the new `FuzzListQuery`
  fuzzes listing query strings and follows every cursor. CI runs both.
- `TestDemoScript` runs `examples/demo.sh` against the real binary twice,
  across a restart, checks every step's outcome in order and then runs
  `ledgerd verify` on the data directory, so the walkthrough cannot drift
  from the API.

### Fast recovery and storage safety (#24)

#### Added

- Checksummed snapshots (`snapshot-<n>.snap`) of accounts, transfers, the
  journal and kept idempotency keys, written in the background via a
  temporary file, fsync, rename and directory fsync. Startup loads the newest
  intact snapshot and replays only the log after it, falling back to an older
  snapshot or the full log when one is damaged. Configured with
  `LEDGERD_SNAPSHOT_EVERY` (default 100,000 records; 0 turns snapshots off).
  A 1 million entry ledger opens in 2.5 s instead of 9.4 s.
- The log is now a directory, `wal/`, of fixed-size segments listed by a
  checksummed `MANIFEST`. A full segment is sealed with a footer carrying its
  record count, length and checksum, so it can be checked and copied on its
  own. Segments whose records all precede the oldest kept snapshot are
  deleted, so disk use follows the snapshot interval. Configured with
  `LEDGERD_SEGMENT_BYTES` (default 64 MiB).
- `ledgerd verify [-json] [dir]` checks a stopped server's data directory
  offline: every checksum in every segment and snapshot, segment continuity,
  every invariant, and that replaying the log reproduces each snapshot
  exactly. Exit 0 sound, 1 damaged, 2 repairable, 3 could not run.
- Metrics: `ledgerd_log_segments`, `ledgerd_log_segments_dropped_total` and
  `ledgerd_snapshots_total`; `log_segments` in stats and the startup log.
- `internal/wal/faultfs`, an in-memory file system that simulates power loss
  by dropping, tearing or reordering unsynced writes and directory changes.
- Tests: thousands of simulated power cuts on the log and on the reference
  model workload; cuts at every byte across segments and snapshots; storage
  failures at every step of a segment roll and a snapshot write; and an
  end-to-end test that kills a loaded server with `SIGKILL` three times,
  restores each time from a copied directory, runs `ledgerd verify` and
  compares every balance with the acknowledged history.
- docs/DESIGN.md documents the segment, manifest and snapshot formats and
  how to restore a copied data directory.

#### Changed

- The invariant checker (startup and `GET /v1/admin/invariants`) checks a
  frozen copy-on-write view of the ledger, so commits continue during a
  scan. On a 10 million entry ledger a commit used to wait 14.8 s for a
  check; it now waits at most a few milliseconds.
- A data directory with the old single-file `ledger.wal` is adopted as
  segment 1 on first start.
- Expired idempotency keys leave the disk with the segments that held them,
  replacing the startup compaction that rewrote the whole log.

#### Fixed

- The data directory and `wal/` were created without fsyncing their parent
  directories, so a power cut could lose a new ledger's whole log, including
  acknowledged transfers.
- Short API keys and an admin token equal to an API key are now rejected
  before the ledger is opened, so a bad configuration no longer creates the
  data directory or replays the log first (#76).
