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
refused), a *normal balance* side, optional limit flags, an optional
`exchange` flag (see [multi-currency transfers](#multi-currency-transfers))
and bounded metadata.
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
`posted + incoming pending` above zero. They are the limits an account
opens with; [balance limits](#balance-limits) can raise, lower or remove
them later as amounts. A change that moves the balance *towards* the limit
is always allowed, so posting, voiding or expiring a reservation can never
be blocked by a limit.

A transfer is the business object a client
creates; journal entries are the immutable records of what happened to it.

| Operation | Entry kind | Effect per posting |
|---|---|---|
| single-phase transfer | `posted` | posted += amount |
| pending transfer | `pending` | pending += amount |
| post (capture) | `post_pending` | pending -= reserved, posted += captured |
| increase a hold | `increase_pending` | pending += increase |
| extend a hold | `extend_pending` | none; the entry has no lines |
| void | `void_pending` | pending -= reserved |
| timeout | `expire_pending` | pending -= reserved |
| reversal | `reversal` | posted += amount on the opposite side |

A transfer has 2 to 64 postings, each a debit or credit to a distinct
account. The postings are in the transfer's currency, or in two currencies
for a conversion, and debits must equal credits in each currency or the
transfer is refused. A conversion is one entry, so a crash cannot leave one
side done; it carries its rate, source and quote time, and its details are
in [multi-currency transfers](#multi-currency-transfers).

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
While a hold is open it can grow and its timeout can move later; see
[changing a hold](#changing-a-hold).

History is immutable: there is no update or delete. A correction is a
reversal: a new transfer with the original postings' sides flipped, linked
both ways (`reverses` / `reversals`). Reversals may be partial and repeated
until the posted amount is used up; each posting tracks `reversed`. A
reversal is an ordinary entry, so limits apply: refunding a merchant that has
already paid out its balance is refused, not silently overdrawn. A reversal
cannot itself be reversed; post a new transfer instead.

### Freezing and closing accounts

An account is `open`, `frozen` or `closed`. Three logged commands move it:

| Command | From | To | Refused with |
|---|---|---|---|
| `freeze_account` | open | frozen | `invalid_state` if already frozen, `account_closed` if closed |
| `unfreeze_account` | frozen | open | `invalid_state` if open, `account_closed` if closed |
| `close_account` | open or frozen | closed | `failed_precondition` unless the posted balance is zero and both pending totals are zero; `account_closed` if closed |

Each takes an optional `reason` (up to 500 characters of printable text),
which the account reports with the time of the change as `status_reason`
and `status_changed_at`.

While an account is **frozen**, every entry that would move money through it
is refused with `account_frozen`: new single-phase or pending transfers in
either direction, posting a pending transfer that has a posting on it (in
full or in part), and reversals that touch it. Releasing a reservation is
not: a void or an expiry only gives back what was reserved, and blocking it
would also lock the money of the other side of the hold. So a hold on a
frozen account stays reserved until it is voided, expires, or the account is
unfrozen and the hold posted. Freezing does not change the account's
`version`, which counts journal entries; a client that needs to know the
status reads it.

**Closing** needs the account to hold nothing: posted debits equal posted
credits and no reservation is open on either side, so no void or expiry can
ever reach it later. A closed account can still be read, listed and have its
statement fetched, but every command that touches it is refused with
`account_closed`, and closing is final.

A status change is not a journal entry. The account keeps every change it
went through, oldest first, each with the journal head when it happened:
entries up to that head came before it, entries after it came under the new
status. A point-in-time read (`as_of_seq=N`) therefore reports the status as
of entry N, and the invariant checker proves that no entry touched an
account while it was frozen (other than a release) or closed. The rules are
checked in `post`, the one place every entry passes through, so batching,
replay and every command kind share them; a status change and the transfers
queued behind it in the same batch apply in order.

### Balance limits

An account has two limits, each an amount in minor units or none, both
measured on the account's normal side:

| Limit | Bounds | Opens at |
|---|---|---|
| overdraft `O` | `available` may not go below `-O` (outgoing reservations count) | 0 with `non_negative`, else none |
| credit `C` | `posted + incoming pending` may not go above `C` | 0 with `non_positive`, else none |

An overdraft limit is a wallet or current account that may go into the red
by up to `O`; a credit limit caps what a debit-normal receivable (a card or
loan balance) may reach, or what a wallet may hold. An account may have
both, bounding its balance on both sides.

The `set_limit` command (`POST /v1/accounts/{id}/limits` with
`{"kind":"overdraft"|"credit","amount":N}`, or `"unlimited":true` to remove
the limit, and an optional `reason`) changes one of them. One of `amount`
and `unlimited` must be given, so a missing field never removes a limit by
accident. Like a status change it is logged, writes no journal entry and
does not change `version`; it is refused with `account_closed` on a closed
account and allowed on a frozen one. The account keeps every change with
the journal head it happened at; a change made at head N has **effective
sequence** N+1: it governs entry N+1 onwards, and a point-in-time read as of
entry N or later reports it. Accounts report their limits as
`limits: {"overdraft", "credit", "effective_seq"}` (null where none is set;
`effective_seq` is that of the newest change, absent while the account has
its opening limits), and `GET /v1/accounts/{id}/limits` returns the limits
with every change: kind, amount (null when removed), effective sequence,
time and reason.

The rule an entry must pass is the same at every moment: it may not take
the balance past the limit in force, nor further past it than it already
was. Lowering a limit below what an account already uses is therefore
allowed, as when a credit line is cut: the account is over its limit and
can only move back towards it (money in, a hold posted for less or voided)
until it is within it again. The check runs in `post`, inside the writer, on
the balance as it stands after every earlier command in the batch, so a
limit change and the transfers queued around it apply in order and two
spends can never both pass against the same headroom. The invariant
checker applies the same rule to every account's history: each entry,
against the balance before it and the limits in force at its sequence.

### How the rules combine

Every entry that touches an account passes the checks in `post` in one
order, posting by posting: the account must exist (`not_found`), its status
must allow the entry (`account_closed`, or `account_frozen` unless the entry
only releases a reservation), it must hold the entry's currency
(`currency_mismatch`), the new totals must not overflow (`overflow`), and
the balance must stay within the limits in force (`limit_exceeded`). The
first failure refuses the whole entry and nothing is written. Status and limit commands are refused only by status:
`set_limit` works on a frozen account so a credit line can be arranged
during a review, and nothing works on a closed one. An account's life
therefore reads as one sequence of logged commands, which
`examples/demo.sh` walks over HTTP (step 10 onwards) and `TestDemoScript`
runs in CI:

| Step | Command | Outcome |
|---|---|---|
| open | `POST /v1/accounts` with `non_negative` | overdraft limit 0 |
| fund | transfer in | balance 200.00 |
| freeze | `POST /v1/accounts/{id}/freeze` | a payout is refused with `account_frozen`; listings show `status: frozen` |
| list | `GET /v1/accounts?metadata[cohort]=...&limit=1`, following `next_cursor` | one account per page, each page as of one entry |
| unfreeze, raise a limit | `/unfreeze`, then `/limits` with `{"kind":"overdraft","amount":5000}` | governs entries from `effective_seq`; a 230.00 payout leaves -30.00 |
| close | `POST /v1/accounts/{id}/close` | `failed_precondition` at -30.00; accepted once settled to zero, after which every command gets `account_closed` |

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
time it ran, the ID or, for a batch, IDs it generated and the entry
sequence it produced),
`expire` (a timeout released a reservation), `reject` (a refused command
that carried an idempotency key, with its error) and `snapshot` (the point a
snapshot was taken at, below). Replay runs each `cmd`
through the same code at the recorded time, so the rebuilt state is
identical, and it checks that each record produces the sequence number the
log says it did. Expiries are records of their own, so recovery never
consults the clock.

On start the log directory is locked with an exclusive `flock` on its
`LOCK` file, so a second process pointed at the same directory refuses to
start instead of interleaving appends; the kernel drops the lock when the
process dies, even on `kill -9`. Then:

- A torn final line (no newline, or a bad checksum on the last line) in the
  newest segment belongs to a batch that was never acknowledged; it is
  dropped and the segment truncated.
- Any other bad line, or a record that fails to replay, means the log was
  damaged after it was acknowledged. Startup stops with an error rather than
  guess which acknowledged transfers to throw away.
- Every invariant (below) is checked before the server accepts traffic.

### Segments

The log is a directory, `wal/` under the data directory, of segment files
and a manifest. Records are numbered from 1 across the whole log, and each
segment is named after the number of its first record
(`00000000000000048211.seg`). Appends go to the newest segment. When a
batch would take it past `LEDGERD_SEGMENT_BYTES` (64 MiB by default) it is
sealed and the batch starts a new one; a batch never spans two segments, so
it is still one write and one fsync. A segment that already holds records
is sealed even if one batch alone is larger than the limit.

A sealed segment ends with a footer line:

```text
#seal <first record> <records> <bytes> <crc32c of those bytes> <crc32c of the line so far>
```

where `<bytes>` is the length of the segment before the footer. Record
lines start with a hex digit, so a footer is never mistaken for a record.
A sealed segment is never written again and describes itself completely:
it can be checked, copied or archived on its own. On start every sealed
segment must end exactly at a footer that matches its contents (first
record, count, length and checksum); a sealed segment cut short, with a
flipped bit, or with anything after its footer stops startup. Only the
newest segment may end in a torn line, which includes a torn footer.

The manifest (`MANIFEST`) lists the segments by first record, oldest
first, and ends with a CRC-32C of everything above it:

```text
ledgerd-wal 1
1
48211
crc 3f2a9c01
```

It is replaced atomically (temporary file, fsync, rename, directory fsync)
and is the authority on which files make up the log. Each segment must
start where the one before it ends; a listed segment that is missing stops
startup. Sealing is ordered so every crash point is recoverable: the footer
is written and fsynced, then the next segment is created and fsynced, then
the manifest listing it is written. On start, a newest segment that is
already sealed means the crash came before the manifest, and Open finishes
the roll; an unlisted, empty segment past the end is removed.

Segments are how the log is compacted. Once a snapshot is written and the
older snapshots are pruned, every sealed segment whose records all come
before the oldest kept snapshot's `snapshot` record is deleted: the
manifest is rewritten first, then the files are removed, so a crash leaves
at most unlisted older files, which the next start removes. Recovery always
starts from one of the kept snapshots, so it never needs those records.
Disk use therefore follows the snapshot interval, not the ledger's age,
and idempotency keys past their retention leave the disk with their
segments (snapshots do not copy expired keys). This replaces the earlier
compaction, which rewrote the whole log at startup to drop expired keys.
With snapshots turned off nothing is dropped. Failing to drop is logged
and retried after the next snapshot or start; it costs disk space only.
`ledgerd_log_segments` and `ledgerd_log_segments_dropped_total` report the
segment count and drops.

A data directory from an earlier version holds the log as one file,
`ledger.wal`. On first start it is adopted as segment 1: hard-linked into
`wal/`, the manifest written, then the old name removed. A crash at any
point of that leaves either the old layout, which is adopted again, or the
new one.

The crash test writes a workload of accounts, single-phase and two-phase
transfers, captures, reversals, an expiry and a keyed rejection, recording
the state after each acknowledged command. It then truncates the log at
every byte offset and recovers each prefix: the result must equal the
state after some acknowledged prefix, never go backwards as the offset grows,
and satisfy every invariant.

A second crash test does the same across segments and snapshots. It writes
a workload with 768-byte segments and a snapshot every three records, keeps
every segment and snapshot file the run produced, and rebuilds the data
directory as a crash would leave it with the log cut at every byte: inside
a record, inside a seal, right after a seal before the next segment exists,
and in a new, empty segment. Each cut is recovered three ways: from the log
alone, from the two newest snapshots the cut log has records for with the
segments before them dropped, and with the newest of those never renamed
(a half-written temporary file in its place) and the two before it present.
The log alone must recover an acknowledged state that never goes backwards;
each version with snapshots must recover exactly what the log alone does.

### Power loss

A truncated log is only one thing a power cut can leave. An fsync covers one
file's data; a new, renamed or removed name is durable only once its
directory is fsynced; and until then, writes may reach the disk partly, in
any order, or not at all. The log and the snapshots go through a small file
system interface (`wal.FS`), and the tests run them on `internal/wal/faultfs`,
an in-memory file system that keeps every file's synced bytes and every
directory's synced entries apart from what changed since. A power cut keeps
the synced state plus a random selection of the rest, chosen per file and per
directory: none of it, all of it, a prefix whose last write may be torn, a
random subset in order, or (for file data) a random subset applied out of
order. A rename survives whole or not at all. The one thing it does not do is
tear a single write into pieces that are not a prefix; the log assumes an
interrupted append leaves a prefix.

Two tests cut the power thousands of times. The log test appends batches,
rolls and seals segments, drops old ones and adopts a legacy single-file log,
with cuts landing inside Open, between a batch's write and its fsync, in the
middle of a roll and inside a drop; every acknowledged record must replay at
its index, followed by at most a prefix of the batch in flight. The ledger
test runs the reference-model workload below and cuts the power in the middle
of concurrent commands, during expiry sweeps, while a snapshot is written in
the background and while segments are dropped. The ledger must open after
every cut, every acknowledged command must still be there with the result it
was given, and the model, fed whatever the recovered log holds, must agree on
every balance, transfer and invariant. Both tests require every kind of loss
to have happened at least once.

This found one real gap: the data directory and `wal/` were created without
fsyncing their parents, so a power cut could take a new ledger's whole log,
acknowledged transfers included, with the directory entry that held it. Directories are now created by
`wal.MkdirAll`, which fsyncs the parent of each one it creates.

Storage errors are fail-stop. If a write or fsync fails, the ledger stops
accepting writes (HTTP 503, `/readyz` fails) and keeps serving reads of the
last durable state. It does not retry the fsync: after a failed fsync the
kernel may already have dropped the dirty pages, so neither the file nor a
retry can be trusted ([PostgreSQL's fsync() surprise](https://lwn.net/Articles/752063/);
Rebello et al., *Can Applications Recover from fsync Failures?*). A restart
recovers from what is really on disk, and clients learn the outcome of the
ambiguous requests by retrying with their idempotency keys. The tests inject
both failures and check exactly that.

They also fail each step of a segment roll in turn (writing the seal,
fsyncing it, creating the next segment, fsyncing it, fsyncing the
directory, writing the manifest, renaming it and fsyncing the directory
after the rename). The batch that needed the new segment is refused, the
ledger stops, and after a restart the keyed retry runs once, the log rolls
again and `ledgerd verify` reports the directory sound. Each step of
writing a snapshot (creating the temporary file, writing it, fsyncing it,
renaming it, fsyncing the directory, pruning old snapshots, dropping
segments) is failed the same way. A snapshot failure is not fatal: the
ledger keeps accepting writes, drops no segment on the strength of the
failed snapshot, takes the next one as usual and restarts to the same state.
Finally the power is cut on faultfs right before each of those snapshot
steps, including between the rename and the directory fsync, where the
rename may or may not survive and the temporary file's own entry was never
synced. Whatever comes back opens to exactly the state acknowledged before
the cut, takes new snapshots past the one cut short and restarts cleanly.

### Snapshots

Replaying every command ever accepted makes restart time grow with the
ledger's age, so the writer also takes snapshots: every
`LEDGERD_SNAPSHOT_EVERY` log records (100,000 by default), after recovery
if it replayed at least that many, and on a clean shutdown. A snapshot holds
everything the state machine holds: each account with its totals and
version, every transfer, the journal, and the idempotency keys still kept
with their stored results.

Between two batches, the writer appends a `snapshot` record carrying the
snapshot's number and the journal head, fsynced like any batch, then copies
the state as of that record. The copy costs a pass over accounts, transfers
and keys (journal entries are immutable and shared); a background goroutine
sorts, encodes and writes it while commits continue. The file goes to a
temporary name, is fsynced, renamed to `snapshot-<number>.snap` and the
directory fsynced, so a crash leaves the whole file or none. The two newest
snapshots are kept.

On start, Open loads the newest snapshot that reads back intact, then reads
the log: records up to that snapshot's `snapshot` record are only
checksummed, not decoded or run, and replay starts after it. The snapshot
record must carry the snapshot's head. A damaged snapshot is skipped for an
older one, or for a full replay while the log still starts at record 1.
The segments before the oldest kept snapshot may have been dropped, so if
the log starts later than record 1 and no snapshot reads back intact,
startup stops. A snapshot whose record is not in the log (a log restored
from an older copy, say) stops startup, because the two disagree about
history. Failing to write a snapshot is logged and counted in
`ledgerd_snapshots_total{result="failed"}`; it costs restart time, never
data.

The file is binary: the magic `LDGSNAP1`, then frames of a big-endian
`uint32` length, a CRC-32C and up to about 64 KiB of items. Items (tag byte,
varints and length-prefixed strings) never span frames. A header gives the
format version, snapshot number, head and item counts; accounts, transfers,
entries and keys follow; a footer counts the frames before it. Loading
checks every frame's checksum, every count, that entries are numbered 1 to
head and each is listed by exactly one transfer, and refolds each account's
entries, which must reproduce its stored totals and version; history rows
are rebuilt by that fold. The tests flip the low and high bit of every byte,
and cut the file at every byte, of a snapshot with every kind of state, and
each damaged copy is refused.

On a 4-vCPU 2.1 GHz Xeon virtual machine, opening a ledger of 1 million
single-phase transfers takes 9.4 s replaying the log and 2.5 s from a
snapshot at the head; a 10,000-entry tail after the snapshot adds about
0.15 s of replay (`BenchmarkRecovery`). Most of what remains grows with the
ledger, not the tail: decoding the 157 MB snapshot (about 1.7 s, much of it
building maps and garbage collection) and the startup invariant check (about
0.75 s, measured before the checker moved to a frozen view). Both shrink once state lives on disk rather than wholly in memory.

### Checking a data directory offline

`ledgerd verify [-json] [dir]` checks a data directory without starting the
server and without writing to it, so it can run against a backup, a copy
taken off a dead machine, or a stopped server before an upgrade. It takes
the log's `LOCK` (opened read-only, never created), so it refuses a
directory a server has open and no server can start while it runs.

It reads every file and checks:

- every segment: each record's CRC-32C, the seal's counts and checksums,
  that segments follow each other in the manifest without a gap, that
  every sealed segment ends at its seal, and which segment files the
  manifest does not list;
- every snapshot: frame checksums, counts and the refold of each account's
  entries (everything Open checks), and then every invariant of the state
  it holds;
- the log against the snapshots: replay starts at record 1 while the log
  still has it, otherwise at the oldest intact snapshot whose record is in
  the log, and at each later `snapshot` record the replayed state must
  equal that snapshot exactly (accounts with history rows, transfers,
  journal, open reservations and their timeouts, and every idempotency key
  the snapshot kept);
- every invariant of the fully replayed state.

Damage does not stop the checksum pass: replay stops at the first problem,
but every later segment is still read, so one report lists everything.

The verdict is one of three exit codes:

| Exit | Verdict | Meaning |
|---|---|---|
| 0 | sound | Every check passed; the server would start without changing a file. |
| 1 | damaged | The server would refuse to start (a damaged sealed segment, a gap, a bad manifest, a newest snapshot the log does not mark, no snapshot standing in for dropped segments), or a file passes its checksums but breaks an invariant or disagrees with the log. |
| 2 | repairable | Nothing acknowledged is lost and the server would start: a torn final write (never acknowledged, since acknowledgement follows the fsync), a roll, drop or upgrade a crash interrupted, a leftover temporary file, or a damaged or unmarked snapshot that startup skips. A normal start repairs the log; a skipped snapshot is replaced by the next one written, or can be deleted. |
| 3 | | Verify could not run: bad usage, a directory in use, or nothing to check. |

`-json` prints the full report: each segment with its records, size, seal
and problem; each snapshot with its head, the log record that marks it and
whether it matched; and the replayed head and counts.

### On-disk formats

A data directory holds:

```text
<data>/
  snapshot-00000000000000000041.snap       newest two snapshots, named by snapshot number
  snapshot-00000000000000000042.snap
  snapshot-00000000000000000043.snap.tmp   only while a snapshot is being written
  wal/
    LOCK                                   flock target; holds no data
    MANIFEST                               which segments make up the log
    MANIFEST.tmp                           only while the manifest is replaced
    00000000000000048211.seg               sealed segment (records 48211 ...)
    00000000000000052977.seg               newest segment, takes appends
```

Every name is fixed width, so sorting names sorts by number. All checksums
are CRC-32C (Castagnoli); in text files they are 8 lower-case hex digits.

**Segment** (`wal/<first record, 20 digits>.seg`). Text, one record per line,
then a footer once sealed:

```text
<crc32c of json> <json record>\n          repeated
#seal <first> <records> <bytes> <crc32c of the first <bytes> bytes> <crc32c of this line up to here>\n
```

A record is a JSON object with `"t"` naming its type and only the fields
that type uses. Unknown fields are refused. A record's JSON is at most 16
MiB; readers take lines up to 1 MiB in place and assemble longer ones in a
buffer of their own, so replay memory follows the longest record actually
in the log.

| `t` | Fields | Written when |
|---|---|---|
| `header` | `v` (log format version, 1) | first record of a new log |
| `cmd` | `time`, `cmd` (the command as accepted), `seq` (last entry it produced, if any), `id` (the ID it generated for a new transfer or reversal), `ids` (the IDs it generated for a batch's transfers, in order), `idem` (`scope`, `key`, request `hash`) if it had a key | a command is accepted |
| `reject` | `time`, `idem`, `err` (`code`, `message`, and `item`, the refused transfer's index, for a batch) | a command with an idempotency key is refused |
| `expire` | `time`, `id`, `seq` | a pending transfer times out |
| `snapshot` | `time`, `snap` (snapshot number), `seq` (journal head) | a snapshot is taken |

**Manifest** (`wal/MANIFEST`). Text:

```text
ledgerd-wal 1
<first record of each segment, oldest first, one per line>
crc <crc32c of every line above>
```

**Snapshot** (`snapshot-<number, 20 digits>.snap`). Binary:

```text
"LDGSNAP1"
frame*      uint32 big-endian payload length | uint32 big-endian crc32c of payload | payload
```

A payload is a run of whole items; a frame is closed once it passes 64 KiB,
and a reader refuses one over 4 MiB. Inside items, `u` is an unsigned
varint, `i` a signed (zig-zag) varint, `str` a `u` length then bytes, `bool`
one byte 0 or 1, `time` a `bool` (set) then, if set, `i` Unix seconds and
`u` nanoseconds, and `meta` a `u` count then key and value `str` pairs in key
order. Items, in file order:

| Tag | Item | Fields |
|---|---|---|
| `H` | header, first | `u` format version (7), `u` snapshot number, `u` journal head, `time` taken, `u` counts of accounts, transfers, entries and keys |
| `A` | account, by ID | `str` ID, `str` currency, `i` exponent, `str` normal balance, `bool` non-negative, `bool` non-positive, from format 7 `bool` exchange, `meta`, `time` created; `i` debits posted, credits posted, debits pending, credits pending; `u` version; `u` count then per status change `u` journal head, `time`, `str` status, `str` reason; `u` count then per limit change `u` journal head, `time`, `str` kind, `i` amount (-1 for none), `str` reason |
| `T` | transfer, by ID | `str` ID, state, currency; `u` posting count then per posting `str` account, `str` direction, `i` amount, posted, reversed, and from format 7 `str` currency (empty for the transfer's own); `str` reference, `meta`, `time` created, `time` expires, `str` reverses; `u` count then `str` reversal IDs; `u` count then per entry `u` sequence, `str` kind; from format 5 a balancing byte (0 none, else bit 0 `balance_debit`, bit 1 `balance_credit`) followed, when not 0, by `i` requested amount and `i` amount; from format 7 a rate: `bool` set, then if set `str` base, quote, rate and source and `time` quoted, present exactly when a posting names another currency |
| `E` | journal entry, by sequence from 1 | `u` sequence, `time`, `str` kind, transfer ID, currency; `u` line count then per line `str` account, `str` direction, `i` posted, `i` pending, and from format 7 `str` currency (empty for the entry's own); from format 6 `time` expires, set only on an `extend_pending` entry, which alone has no lines; from format 7 a rate as in `T`, only on the entry that creates a multi-currency transfer and equal to that transfer's |
| `I` | idempotency key, oldest first | `str` key, 32-byte request hash, `time` stored, flags byte (1 error, 2 account result, 4 transfer result, 8 batch result, 16 error names a batch item); then if flagged: `str` code and message, and `u` item index if flag 16; an account as in `A` with `u` version, the four totals, `u` as-of sequence, then `str` status, `str` status reason and `time` changed, then `i` overdraft and credit limits (-1 for none) and `u` effective sequence; a transfer as in `T`; `i` count of reservations expired; finally `u` number of batch transfers if flag 8 |
| `B` | one transfer of a batch result, right after its `I` item, in batch order | a transfer as in `T` |
| `Z` | footer, alone in the last frame | `u` number of frames before it |

Each format carries a version: the log's `header` record, the manifest's
first line and the snapshot's `H` item. A reader refuses a version it does
not know, so a changed layout must come with a new version number. Snapshot
format 2 added account status, format 3 limit changes, format 4 batch
results, format 5 balancing records, format 6 hold increases and
extensions (`increase_pending` and `extend_pending` entries, and the
timeout an extension sets) and format 7 multi-currency transfers (the
exchange flag, posting and line currencies, and rates); format 1 and 2
files, which lack those fields, still load with every account open and at
the limits its flags give, and format 3 to 6 files, written before
batches, balancing transfers, hold changes or multi-currency transfers
existed, load as they are; a file older than 6 holding either new entry
kind is refused. A batch result is spread
over `B` items because thousands of transfers can be more than one frame
holds. New command kinds in `cmd` records keep
the log at version 1: an older binary refuses a log holding a command it
does not know, as an unknown field, rather than misreading it.

### Restoring a copied directory

A data directory is self-contained, so restoring a server is copying its
directory to another place or machine and starting `ledgerd` on the copy.
The copy must be taken while no server has the directory open (after a
clean stop, or after the process or machine has died): the log, manifest
and snapshots are written at different moments, so a copy taken under a
running server can mix files from different points in time. Online backups
come with the operations work later in the roadmap. Then:

1. Run `ledgerd verify` on the copy. `0` or `2` means it holds every
   acknowledged write; `1` means it does not, so restore an older copy.
2. Start `ledgerd` on the copy. Startup completes any interrupted roll,
   drop or snapshot and removes the debris.
3. Clients retry every request that did not get an answer with the same
   `Idempotency-Key`. A request that reached the log replays its original
   answer; one that did not runs now. Either way it applies exactly once.

The end-to-end test (`cmd/ledgerd/e2e_test.go`) does exactly this with the
real binary. Each of three rounds runs a `ledgerd` process with 4 KiB
segments and a snapshot every 500 records, loads it from 8 concurrent
clients with transfers, holds that are posted or voided, and reversals, and
sends it `SIGKILL` once it has written a new snapshot and dropped segments,
which is usually while the next snapshot, roll or drop is in progress. The
directory is copied, `ledgerd verify` must exit 0 or 2 on the copy, and the
next round's server starts on the copy. That server must replay every
acknowledged request, with the same transfer ID, when it is resent with its
key; the request each client had in flight is resent and may be either a
replay or new. Every account's four totals must then equal the totals
folded from the acknowledged history, and the invariant checker must be
clean. After the last round the copy is stopped cleanly, `verify` must
report it sound, and a restart must give the same balances. Across runs the
kills left torn tails, half-written snapshots, interrupted rolls, leftover
manifests and segments an interrupted drop had not yet removed, all
reported as repairable.

### Richer transfers across a kill

`TestRicherTransfersSurviveKillNine` (`cmd/ledgerd/richer_e2e_test.go`)
runs this week's three motivating cases through the real binary, first one
step at a time with every balance checked after each step, then under load
across crashes:

- **A split payout.** One batch captures a 100.00 order into a
  non-negative escrow account, pays 60.00 and 25.00 to two sellers and 5.00
  of VAT, and sweeps what is left to the platform with a `balance_debit`
  transfer asking for 10,000.00; the sweep moves exactly 10.00 and the five
  entries are consecutive. A second batch promises 55.00 out of a 50.00
  capture and is refused with `limit_exceeded` at item 2; the capture and
  the first payment are not applied.
- **A hotel hold that grows.** A 500.00 card holds 200.00 at check-in, the
  minibar adds 60.00 with an increase, a 300.00 upgrade is refused because
  the card has only 240.00 left, a late checkout extends the hold by two
  days, and checkout posts 245.00 of the 260.00 held. The transfer's
  entries are `pending`, `increase_pending`, `extend_pending`,
  `post_pending`, and the card ends at 255.00.
- **A USD to EUR conversion.** 100.00 USD at 0.9234 with a 1.00 USD fee is
  one transfer with five postings and one entry, its rate and quote time
  (in UTC) recorded. The same conversion two cents short of the rate is
  refused with the exact conversion in the message, and nothing moves.

Then 6 clients run payouts, stays (hold, increase, extend, post) and
conversions on accounts of their own, two of them converting only, and the
server is killed with `SIGKILL` twice once it has acknowledged 600 more
requests and written a new snapshot, so every kill lands with conversions
and usually a batch unanswered. Each time the directory is copied,
`ledgerd verify` must exit 0 or 2 on the copy and the next server runs on
it. On that server every acknowledged request, walk-through and load alike,
must replay its original answer byte for byte; the request each client had
unanswered is resent and may be a replay or run now. Then every account's
four totals must equal the acknowledged requests, debits must equal credits
in USD and in EUR, the invariant checker must be clean, and listing every
transfer through `fx_usd` must find exactly the acknowledged conversions,
each posted with one entry, both currencies balanced and the rate. A batch
or conversion written in part would leave a balance, a currency sum or that
listing wrong. Across runs, unanswered batches, conversions, holds,
increases, extensions and posts were each found both written and not
written. After a clean stop the copy verifies as sound and restarts to the
same balances. The test takes about 1.3 s, or 8 s under `-race`.

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

## Atomic batches

`POST /v1/transfers/batch` takes up to 8,190 transfers, the number of
events TigerBeetle accepts in one request, and commits them as one unit.
It is TigerBeetle's chain of linked events: the transfers run in the order
given, each seeing the balances, versions and holds the ones before it
left, so a batch can fund an account and spend from it, and a precondition
on a later transfer is checked against the version the earlier ones
produced. If any transfer is refused, none of them happens.

The batch is one `create_batch` command. Validation checks every transfer
as it would a single one, then that the batch fits in one log record: the
encoded command, plus 67 bytes for each generated ID at the longest an ID
can be, plus 4 KiB for the record's other fields, must be at most
`MaxBatchBytes` (16 MiB, the log's record limit). A batch that passes can
therefore always be logged, and the writer never meets a record it cannot
write. On success the record is a `cmd` with `ids`, the generated IDs in
order, and `seq`, the last of the batch's consecutive entries; replay runs
the batch with those IDs and checks it ends at that entry. A refused batch
with an idempotency key is a `reject` record whose error carries `item`.

All or nothing inside a group commit needs a way to take the batch back out
of a transaction that other commands share. A single command never needs
one, because it checks everything before it changes anything. A batch
cannot: its third transfer may depend on what its first did. So the batch
runs under a *savepoint*. While one is set, the first time the transaction
hands out an account or replaces a transfer it keeps its own copy as it was
(or notes that it had none), and the savepoint records the lengths of the
append-only parts: entries, history rows, new timeouts, new transfer IDs,
and the open count. Rolling back restores those copies, deletes what was
new, and cuts the append-only parts back. Commands before and after the
batch in the same group commit are unaffected and see none of it, not even
its generated IDs. The cost is one map entry and one account copy per
account the batch touches, paid only by batches.

The response lists one result per transfer. On success each is `created`
with the transfer. On failure the status and error are the refused
transfer's (`limit_exceeded` 422 for an overdraft, for example), the error's
`item` is its index and its message starts `transfers[i]: `, and the
results list it as `failed` and every other transfer as `not_applied`.
Validation errors about one transfer name it the same way. One
`Idempotency-Key` covers the whole batch: a retry returns every transfer of
the original, or the original refusal naming the same transfer, even once
the batch would succeed.

### What a batch costs

`BenchmarkBatch` (`internal/ledger`) submits batches of 1, 100 and 8,190
two-posting transfers from one client, back to back, into 64 accounts, and
compares them with a plain `create_transfer`. Each command is its own group
commit, so on disk every batch is one log write and one fsync, which the
benchmark checks. Snapshots are off, so it measures the commit path alone.
`BenchmarkBatchHTTP` (`internal/api`) sends the same commands through the
HTTP handler without a network, so it adds authentication, JSON decoding
and the response. Allocations and bytes are counted across the whole
process while the timer runs, writer and background goroutines included.
Medians of three 3-second runs on a 4 vCPU 2.1 GHz Xeon VM, where an fsync
takes about 0.2 ms:

| Command | In memory | On disk | Over HTTP (in memory) | Allocs per transfer (memory / HTTP) | Heap bytes per transfer (memory / HTTP) | Log bytes per transfer |
|---|---|---|---|---|---|---|
| `create_transfer` | 54,000 /s | 3,650 /s | 19,800 /s | 26 / 90 | 4,200 / 14,600 | 270 |
| batch of 1 | 45,700 /s | 4,170 /s | 17,200 /s | 34 / 109 | 4,950 / 15,900 | 286 |
| batch of 100 | 155,000 /s | 82,200 /s | 93,100 /s | 7.5 / 12.5 | 3,640 / 5,310 | 168 |
| batch of 8,190 | 164,000 /s | 156,000 /s | 103,000 /s | 6.1 / 10.1 | 4,000 / 6,550 | 167 |

A batch of 1 costs a little more than a plain transfer (eight allocations
and about 3 µs for the batch wrapper and its savepoint), so a client with
one transfer should send it on its own. From 100 transfers up, the cost per
transfer stops depending on the round trip to the writer and the fsync: a
batch of 8,190 on disk runs as fast as in memory, about 40 times one client
sending single transfers, and its log record takes 167 bytes per transfer
against 270, since the command's fields are written once for the batch.
What is left per transfer, about 6 µs and 4 KB, is mostly what the ledger
keeps: the journal entry and its lines, two history rows, the transfer and
its index entries, plus the record's JSON. Over HTTP, decoding and encoding
add about 4 µs and 2.5 KB per transfer at 8,190; a full batch takes about
87 ms to commit, 1.1 MB in and 2.9 MB out.

## Balancing transfers

A transfer may carry `balance_debit`, `balance_credit` or both
(TigerBeetle's `balancing_debit` and `balancing_credit`). The amount given is
then a maximum: the transfer moves the lesser of it and what each flagged
account can take without crossing its limits. A sweep asks for more than an
account could hold and moves whatever is there; a loan repayment asks for
the payment and stops at what is owed. Only two-posting transfers can
balance, since cutting one leg of a larger transfer has no single answer;
validation refuses others with `invalid_argument`.

The room is measured on the account's normal side, with a limit that is not
set counting as zero:

| Posting on the account's | Moves | Room |
|---|---|---|
| normal side (raises the balance) | `posted + incoming pending` up | `C - (posted + incoming pending)` |
| other side (lowers the balance) | `available` down | `available + O` |

Both quantities move by exactly the posting's amount whether it posts or
reserves, so the rule is the same for a balancing hold, and open
reservations count against the room of later balancing transfers. Counting
an unset limit as zero is TigerBeetle's rule (a balancing debit never takes
the debit account's debits past its credits), and it holds on either normal
balance: a credit-normal wallet with no limits is swept down to zero, and a
debit-normal loan with no limits is repaid down to zero. Where a limit is
set, the transfer may use all of it, overdraft included. The room is
computed in saturating arithmetic, so limits near the int64 range cannot
overflow it.

The flagged accounts are checked for existence, status and currency first,
with the usual errors, so a frozen account with nothing in it is
`account_frozen`, not a limit error. If the room is zero or less the
transfer is refused with `limit_exceeded` ("nothing to balance against");
ledgerd never writes an entry for zero. Only the flagged side is cut: the
other account still passes every check in `post`, so a balancing debit into
an account at its credit limit is refused as usual. Inside a batch a
balancing transfer sees what the transfers before it did, so "fund, then
sweep" is one record.

The transfer's postings hold the amount that moved, and `balancing`
(`balance_debit`, `balance_credit`, `requested_amount`, `amount`) records the
request; it is absent on other transfers. The amount is not in the log:
replay recomputes it from the same state, so the log, a snapshot and a
keyed retry (which returns the original result even after balances change)
all agree. The invariant checker refuses a balancing record its postings
do not bear out (two postings, opposite directions, both of `amount`, which
is positive and at most `requested_amount`, and never on a reversal).

## Changing a hold

A hold (a pending transfer) can change in two ways while it is open, both
logged commands with an `Idempotency-Key` like any other, both recorded in
the transfer's `entries` and in the journal:

- **Increase** (`POST /v1/transfers/{id}/increase`, `increase_pending`)
  reserves more, as when a hotel raises a card hold for an extra night. The
  body is shaped like a partial post: `amount` for a two-posting transfer,
  or `postings` naming how much more each posting reserves, balanced, at
  least one. The entry has a line for each posting that grows, with only a
  pending amount, and each posting's `amount` grows by it. The lines go
  through `post` like a new hold's: the accounts must exist, be open and
  hold the currency, no total may overflow, and no limit may be crossed, so
  an increase past what a wallet has is `limit_exceeded` and changes
  nothing. A balancing hold cannot be increased (`invalid_state`): its
  amount is what its limits allowed when it was made, and a new balancing
  hold takes whatever more there is. Posting the hold later captures up to
  the grown amount, and a void or expiry releases all of it.
- **Extend** (`POST /v1/transfers/{id}/extend`, `extend_pending`) moves the
  timeout to `timeout_seconds` (1 s to 30 days) from now. The new timeout
  must be later than the one the hold has; timeouts never move earlier
  (`invalid_state`), and voiding is the way to release early. An extension
  moves no money, so its entry has no lines: no balance, account version or
  statement row changes. The entry carries the new timeout in
  `expires_at`, the one entry kind that does. The old timeout stays in the
  expiry heap, where the sweep skips it because it no longer matches the
  transfer, and a snapshot schedules only the current one.

Both are refused with `not_found`, `invalid_state` (not pending) or
`transfer_expired`, and while any of the hold's accounts is frozen
(`account_frozen`): freezing stops a reservation from growing or lasting
longer, while a void or expiry still releases it. The sweep runs before
every group commit, so a hold whose timeout has passed is already expired
when an increase or extension reaches it. Neither can appear inside an
atomic batch, which holds new transfers only.

The invariant checker ties a hold to its entries: each listed entry exists,
belongs to the transfer and has the kind listed; increases and extensions
follow the `pending` entry and come before any post, void or expiry; each
posting's `amount` equals what the `pending` and `increase_pending` entries
reserved on its account; each extension's timeout is later than the one
before; and an extended hold expires at its last extension's timeout. Only
`extend_pending` entries have no lines and a timeout.

## Multi-currency transfers

A currency conversion is one transfer, so it is one journal entry and one
log record: a crash can never leave the USD side done and the EUR side not.
Its postings are in two currencies. `currency` is the transfer's own (the
*base*), and a posting in the other (the *quote*) says so with its own
`currency`. A transfer may name at most two; a three-way conversion is two
transfers in an [atomic batch](#atomic-batches).

```json
{"currency": "USD",
 "postings": [
   {"account": "alice_usd", "direction": "debit",  "amount": 10000},
   {"account": "fx_usd",    "direction": "credit", "amount": 10000},
   {"account": "fx_eur",    "direction": "debit",  "amount": 9234, "currency": "EUR"},
   {"account": "alice_eur", "direction": "credit", "amount": 9234, "currency": "EUR"}],
 "fx": {"rate": "0.9234", "source": "ecb", "quoted_at": "2026-11-12T09:00:00+01:00"}}
```

**Balanced per currency.** Debits equal credits in USD and, separately, in
EUR. Nothing ever adds cents to euro cents, so the per-currency totals the
invariant checker sums across accounts stay exact, and every account still
holds one currency: a posting is refused with `currency_mismatch` unless
its currency is its account's.

**The rate is recorded.** `fx` is required when there are two currencies
and refused when there is one. `rate` is how many major units of the quote
one major unit of the base buys, as a plain decimal string (never a JSON
number, a fraction or an exponent: the ledger stores exactly what was
sent), `source` says where it came from, and `quoted_at` when; it is kept in
UTC. The transfer and the entry that creates it carry it as
`{"base","quote","rate","source","quoted_at"}`; later entries of the same
transfer (a post, void, expiry, increase) do not repeat it.

**Exchange accounts.** An account opened with `"exchange": true` is the
ledger's position in one currency on the far side of conversions: a USD
exchange account takes in the dollars a customer sells, a EUR one pays out
the euros. The rate is checked against what moves through the exchange
accounts, not against every posting, so a fee or a spread can sit on
another account in either currency within the same entry. In each currency
the exchange accounts' net movement (credits minus debits) must be
non-zero, the two nets must have opposite signs, and the quote net must be
the base net times the rate, rescaled between the currencies' minor units,
rounded either way to a whole unit: 100.01 USD at 0.9234 is 92.349234 EUR,
so 92.34 and 92.35 pass and 92.36 does not. Any rounding rule a client uses
passes; a stale amount or a wrong rate does not. The arithmetic is exact
(`math/big`); the refusal is `invalid_argument` with the exact conversion
in its message. The exchange accounts' balances are the ledger's open
currency position, and their statements are its conversion history.

**Holds, posts and reversals.** A multi-currency hold reserves in both
currencies and a void or expiry releases both. Amounts a client names for
a partial post, an increase or a partial reversal must balance in each
currency and agree with the rate in the same way; taking what is left
(posting a hold in full, reversing the remainder) is not checked again,
because the parts already checked fix it, and rounding on each part may
leave a remainder a unit off the rate. A reversal carries the original's
rate when it sends back both currencies; reversing only a fee in the base
currency is an ordinary single-currency reversal; reversing only one side
of the conversion is refused. A balancing transfer has two postings, so it
is never multi-currency.

A transfer is listed under both its currencies (`GET
/v1/transfers?currency=EUR` finds a USD to EUR conversion).

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

### Secondary indexes

Reads that are not by ID go through ordered secondary indexes
(`internal/ledger/index.go`): accounts by currency and by metadata
`key=value` pair, and transfers by state, currency, posting account,
reference and metadata pair, plus every account and every transfer. Each
index maps a key to an ordered set. Accounts are ordered by ID. A transfer
is identified by the sequence number of the journal entry that created it,
which is unique, never changes and survives restarts, so a member costs
8 bytes and sets come out in creation order; the entry at that sequence
names the transfer.

A set is a list of sorted chunks of at most 256 keys. Appending past the
last key, which is how transfers arrive, fills the last chunk and then
starts a new one, so sets built in order are packed full; inserting or
removing elsewhere moves at most one chunk's keys, a chunk that fills splits
in two, and one that falls below a quarter full merges into a neighbour.

`txn.commit` updates the indexes with everything it publishes, under the
write lock, so a reader holding the read lock sees indexes that agree with
the accounts and transfers. Only a transfer's state can change after it is
created (its currency, accounts, reference and metadata are fixed), so a
post, void or expiry moves one member between two state sets. Replay goes
through the same commit, so the log rebuilds the indexes as it replays.

Snapshots do not store the indexes: an index read from disk could not be
trusted without checking each member against its record, which costs the
same as building it. Loading a snapshot rebuilds them from the accounts (in
ID order, as stored) and the transfers (in the order of the entries that
created them), so every member is an append and nothing is sorted; the
three groups of transfer indexes, which share no sets, are filled on three
goroutines. On the 4-vCPU 2.1 GHz Xeon VM, rebuilding the indexes of 1
million entries (955,224 transfers over 10,000 accounts) takes about
0.28 s and they take 47 bytes of heap per transfer (`BenchmarkIndexes`).
Keeping them up to date costs about 1.5% of the CPU on the in-memory
commit path.

`Ledger.CheckIndexes` compares every index with one built by a full scan.
The tests run it wherever they check invariants (the reference model,
crash, power-cut and recovery tests), `ledgerd verify` runs it on the
replayed state, and verify's comparison of a snapshot with the log covers
the indexes too. Unlike the invariant checker it holds the read lock for
the whole scan, so it is for tests and offline checks.

### Listing accounts

`GET /v1/accounts` pages through accounts in ID order (`Ledger.ListAccounts`,
`internal/ledger/list.go`). Filters are `currency=` and any number (up to 8)
of `metadata[key]=value` pairs, and an account must match all of them. The
listing walks the smallest index set that every match must be in (all
accounts, the currency's set, or a metadata pair's set) and checks the other
filters on each account; a filter value no account has answers at once with
an empty page.

A page is read under one read lock, so every balance on it is as of the same
journal entry, returned as `as_of_seq`. It resumes after the last ID the
previous page examined. Accounts are never deleted and their ID, currency
and metadata never change, so a walk that runs while other clients write
returns every matching account that existed when it began exactly once, in
ID order; an account created during the walk appears if and only if its ID
sorts after the position the walk had reached. `has_more` is exact when a
page fills: the listing looks one match further before it answers.

`limit` is 1 to 1,000 (default 100). One request examines at most
`max(16 × limit, 4096)` candidates, so a combination of filters that rarely
matches cannot hold the read lock for long: when the budget runs out the
page comes back short, possibly empty, with `has_more` and a cursor to
continue from. On the 4-vCPU 2.1 GHz Xeon VM, with 100,000 accounts, a page
of 100 takes about 20 µs unfiltered and 17 µs by currency; two filters that
match 100 accounts together out of 25,000 scan the full budget in about
0.48 ms per page (`BenchmarkListAccounts`).

The position is carried in an opaque cursor (`internal/api/cursor.go`): a
version byte, the listing it belongs to, an 8-byte SHA-256 prefix of the
canonical filter string, the position, and a 16-byte HMAC-SHA256 over all of
it, base64url-encoded. A cursor edited by a client, used with different
filters or for another listing, or signed with another key is refused with
`invalid_argument`. The `limit` may change between pages. The signing key is
`LEDGERD_CURSOR_SECRET` when it is set; otherwise it is derived from the set
of API keys, so cursors survive restarts without more configuration and stop
working (clients start the listing again) when the key set changes. Unknown
or repeated query parameters are refused, so a misspelt filter is an error
rather than a listing of everything.

### Listing transfers

`GET /v1/transfers` pages through transfers in the order they were created
(`Ledger.ListTransfers`), oldest first, or newest first with `order=desc`.
Filters are `account=` (a posting on that account), `state=` (`pending`,
`posted`, `voided` or `expired`), `currency=`, `reference=` and up to 8
`metadata[key]=value` pairs; a transfer must match all of them. As with
accounts, the listing walks the smallest index set every match must be in
and checks the other filters on each transfer, and a filter value no
transfer has answers at once with an empty page. A page holds whole
transfers, as `GET /v1/transfers/{id}` returns them, and `as_of_seq`.

The position is the sequence number of the journal entry that created the
last transfer the page examined, the same 8-byte key the indexes order
transfers by. Transfers are never deleted, and only their state changes
after they are created, so a walk returns each transfer at most once and in
order, and returns every transfer that existed when the walk began and
matched its filters for the whole walk. A transfer whose state changes
mid-walk is listed if it matched when its page was read: a hold posted
during a `state=pending` walk may be missed by both that walk and a
`state=posted` walk that had already passed it. Posted, voided and expired
are final states, so walks of those never miss anything. An oldest-first
walk reaches transfers created after it began; a newest-first walk starts
at the head when its first page is read and never does. The order picks the
cursor's listing kind, so a cursor from one order is refused by the other,
and leaving `order` out is the same listing as `order=asc`. Limits and the
scan budget are the same as for accounts.

`reference=` is the lookup by external reference: the walk reads only the
reference's own index set, so finding the transfers that carry a payout or
invoice ID costs the number of such transfers, not a statement scan of
their accounts. On the 4-vCPU 2.1 GHz Xeon VM with 200,000 transfers among
1,000 accounts (`BenchmarkListTransfers`), a page of 100 takes about 17 µs
unfiltered in either order, 7 µs for one account's transfers and 14 µs for
pending ones (one in ten); a reference carried by 20 transfers is answered
in about 1.2 µs.

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

1. every journal entry balances in each currency, posted and pending; it
   spans at most two currencies, carries a rate exactly when it creates a
   transfer in two, for that pair, and a posted or pending one moves through
   exchange accounts what its rate says (see
   [multi-currency transfers](#multi-currency-transfers));
2. per currency, total debits equal total credits across all accounts;
3. every account's totals equal the fold of the entries that touch it, and
   its running balances and version agree with that fold;
4. no entry took an account past the balance limit in force at its
   sequence, or further past it than the account already was; every
   limit history is in journal order with known kinds and amounts; and the
   account's totals are not past its current limit beyond where its last
   entry left them;
5. every account's pending totals equal the open pending transfers' postings
   to it, and the count of open transfers matches;
6. every transfer's posted and reversed amounts agree with its postings and
   its reversals, a balancing transfer's record agrees with its two
   postings, every posting is in its account's currency, and a
   multi-currency transfer has the rate of the entry that created it;
7. every transfer's entries exist and are its own, every hold reserves on
   each posting exactly what its `pending` and `increase_pending` entries
   reserved, it was increased or extended only while open, and it expires at
   its last extension's timeout (see [changing a hold](#changing-a-hold));
8. every account's status history follows the rules (open to frozen and
   back, open or frozen to closed, never out of closed, in journal order),
   no entry touched an account while it was closed or, other than a void or
   expiry, while it was frozen, and a closed account holds nothing.

The checker is linear in the journal size, but commits do not wait for it.
It checks a frozen, copy-on-write view of the committed state as of the
journal head when it started, and reports that head
(`checked_through_seq`). Taking the view costs O(1) under the write lock:
the journal and the list of account IDs are append-only, so their prefixes
need no copy and no lock. What a commit changes in place, an account's
totals, version, history length, status history and limit history, or which version of a transfer the map
points at, it first saves into the view, once per account or transfer, while
a check runs. Committed transfers are never modified, so saving one is
saving a pointer. The checker reads accounts and transfers through the view
in chunks of 1,024 under the shared lock and does all the folding and
comparing outside it, so a commit waits for one chunk at most. Transfers are
enumerated from the journal prefix (each is created by exactly one entry),
which also checks that the transfer map and the journal agree. One check
runs at a time; the extra memory is the saved copies of what was touched
during the check, plus the per-account working set the check always needed.

On a ledger of 10 million entries over 100,000 accounts (4-vCPU 2.1 GHz
Xeon virtual machine, in memory), a full check takes 9.7 s. Single
transfers committed back to back during it took p50 15 µs, p99 274 µs and
at most 2.9 ms (p99 57 µs on an idle ledger), and 435,781 of them finished
while it ran. The previous checker held the shared lock for the whole scan,
14.8 s on the same ledger, and a transfer submitted at its start waited for
all of it (`TestCommitLatencyStaysFlatDuringAudit`, with
`LEDGERD_AUDIT_ENTRIES=10000000`; CI runs it at 200,000 entries).

## Checking the ledger against an independent model

`TestMatchesReferenceModel` runs random workloads against the real ledger:
concurrent batches of single-phase and pending transfers (with overflow-sized
amounts, wrong currencies, unknown accounts and stale preconditions mixed
in), posts, partial posts, voids, reversals, clock advances that expire
reservations, idempotent retries (including concurrent duplicates), key reuse
with different requests, and crash-restarts from a copy of the log with a
torn half-batch appended. The same workload freezes, unfreezes and closes
accounts, sets and removes overdraft and credit limits (often below what an
account already uses), opens new accounts with metadata, and puts
references and metadata on transfers and reversals.

It also sends every richer transfer the API has:

- **atomic batches** of one to three random transfers, some followed by a
  "fund, then sweep" pair whose sweep depends on the funding before it, so
  a refusal at any item must undo the transfers before it;
- **balancing transfers** with `balance_debit`, `balance_credit` or both,
  often sweeps asking for more than there is, on every combination of
  normal side and limit, posted and pending;
- **hold changes**: increases by an amount, by a scaled share of every
  posting or by random per-posting amounts (including ones that overflow),
  and extensions to 1 to 8 seconds from now, which may or may not be later
  than the current timeout;
- **conversions** between USD, EUR and JPY (so the rate rescales between
  exponents both ways) through an exchange account in each currency,
  sometimes with a fee in the base currency, rounded down or up, a few
  units off the rate, or through an account that is not an exchange
  account; and partial posts, increases and reversals of them with amounts
  scaled to keep the rate within rounding or miss it, amounts for only one
  currency, and amounts that balance in sum but not in each currency.

The model has its own rules for each: a batch runs its transfers in order
and puts its maps and journal head back if one is refused, reporting the
item; a balancing transfer moves the lesser of the request and each flagged
account's room, worked out on the normal side with an unset limit counting
as zero; an increase reserves more through the same line checks as a new
hold and is refused on a balancing hold; an extension writes an entry with
no lines and moves only the timeout; and a conversion is checked against
its own `big.Rat` arithmetic and exponent table, not the ledger's
`money` package. Returned and stored transfers are compared in full,
including posting currencies, the rate and its source and quote time, the
balancing record, the timeout, the list of entries with their sequence
numbers and the reversals.

The order the writer chose is read back from the log (every operation carries
an idempotency key, so even refusals are recorded), and a separate
single-threaded model replays the same operations in that order. The model
keeps balances as `big.Int` and shares no code with the ledger; it has its
own status and limit rules and its own journal head, and it keeps each
account's history of totals, status changes and limit changes. After every
round these must agree with the ledger:

- every result code, every returned transfer and every returned account;
- every account's balance, version, metadata, status and limits now, and
  the same as of an earlier journal entry (a point-in-time read checked
  against the model's histories), and its limit history;
- every invariant;
- per currency, across all accounts, debits posted equal credits posted and
  debits pending equal credits pending, summed from what the ledger
  returns;
- the listings. Account listings are walked with every currency and
  metadata filter the workload uses, and transfer listings with a sample of
  filters by state, currency, account, reference, metadata and
  combinations, in both orders, with values nothing has among them. Pages
  change size from one to the next (1 to 1,000) and every page must be as
  of the model's head. Each walk must be exactly the model's answer, found
  by scanning its own maps: the same items in the same order, each as the
  model describes it.

The listings are what prove the secondary indexes against the model rather
than against a scan of the ledger's own state. `TestModelCatchesBrokenIndexes`
runs the workload with the indexes broken after every round the way eight
upkeep bugs would leave them (a state never moved, expiries not moved, only
the first posting indexed by account, reversal references or hold metadata
not indexed, the newest transfer left out, accounts filed under the wrong
currency, a second metadata pair dropped), compares only the listings, and
requires each one to be caught; a control run with nothing broken must come
out clean. Changing the index code itself the same way (voids left in
pending, reversals not indexed by reference) also fails the model test with
`CheckIndexes` turned off.

The test also asserts that the workload reached every outcome (each error
code including `account_frozen` and `account_closed`, each operation
including every lifecycle and limit command, batches committed and rolled
back after an earlier item, increases and extensions, balancing transfers
that moved, were cut and found no room, conversions accepted, refused off
the rate and reversed, expiries, replays, crashes), so a passing run cannot
be a vacuous one. Deliberately broken versions of the ledger fail it: the
balancing room on the wrong side, no clamp, a rollback that keeps accounts
or the open count, the rate check off or a unit too lenient, per-posting
amounts summed across currencies, an increase that does not grow the
posting, an extension that does not move the timeout or ignores a frozen
account, increases allowed on balancing holds, and a reversal that drops
the rate. The power-cut and snapshot tests use the same generator and
accounts, so all of these also cross power cuts and snapshots.

## Limits

| Limit | Value | Why |
|---|---|---|
| Postings per transfer | 64 | Bounds the work and record size of one command. |
| Preconditions per transfer | 64 | Same. |
| Metadata | 20 keys, 40-char keys, 500-char values | Stripe's per-key limits, fewer keys. |
| IDs | 64 chars, `[A-Za-z0-9][A-Za-z0-9_.:-]*` | Safe in paths and logs. |
| Reference | 128 chars | |
| Status and limit reason | 500 chars | Same as a metadata value. |
| Balance limit | 0 to 2^63-1 minor units, or none | |
| Idempotency key | 255 printable ASCII chars | Stripe's limit. |
| Idempotency retention | 24 h (1 min to 30 days) | Memory is proportional to keys in the window. |
| Pending timeout | 1 s to 30 days, default 24 h | |
| JSON body | 1 MiB, 16 MiB for a batch | Checked before decoding. |
| Batch | 8,190 transfers, and at most 16 MiB in its log record | TigerBeetle's request size; one record, one fsync. |
| Currencies per transfer | 2 | One rate per transfer; a three-way conversion is two transfers in a batch. |
| Exchange rate | positive plain decimal, 30 digits | Exact, no floats or exponents; `big.Rat` checks it. |
| Rate source | 1 to 64 chars | |
| Statement CSV | 4 MiB, 10,000 lines | |
| Reconciliation | 4 at once, period 366 days, window 31 days, 100,000 movements | Bounds memory per request. |
| Statement page | 1,000 lines | |
| Listing page | 1,000 accounts or transfers, 8 metadata filters, max(16 × limit, 4,096) candidates scanned | Bounds the time a listing holds the read lock. |
| Log record | 16 MiB | Sized for the largest batch; validation guarantees every accepted command fits (tested). Other commands encode to under 256 KiB. |
| Group commit | 1,024 commands per fsync | |
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
