# ledgerd roadmap, November 2026 to October 2027

## Vision

By October 2027 ledgerd covers what hosted ledgers sell (Stripe's balance and Treasury concepts, Modern Treasury Ledgers, the Adyen balance platform and core banking ledgers such as Mambu) and reaches parity with the leading open source ledgers, TigerBeetle for replicated, high-throughput double-entry accounting and Formance for the product layer (a transaction language, multi-ledger tenancy, wallets, payment connectors, flows and reconciliation), at enterprise grade: a Raft-replicated and sharded cluster with automatic failover, storage larger than memory, multi-tenancy, RBAC with SSO for operators, maker-checker approvals, encryption at rest and in transit, a verifiable audit log, backups and point-in-time restore, a web console, SDKs in Go, TypeScript, Python and Java, migration tools and a Kubernetes deployment. It stays a single static Go binary on the standard library and wins where a small codebase can: a container image under 20 MB, cold start under 100 ms from a snapshot, lower resident memory per million accounts than either leader and competitive throughput and p99 latency, all measured against both leaders in a published, reproducible benchmark that is re-run every quarter and enforced by a CI regression gate.

## Benchmarks

Feature parity is tracked against Stripe, Modern Treasury, Formance, TigerBeetle, the Adyen balance platform and Mambu. The performance benchmark can only run against self-hostable products, so ledgerd is measured against TigerBeetle (consensus, throughput, client breadth) and Formance Ledger (product features: Numscript, multi-ledger, wallets, connectors, flows, reconciliation). The first head-to-head run lands in week 7 with explicit targets: container image under 20 MB, cold start under 100 ms from a snapshot, resident memory under 1 GB for 10 million accounts, p99 transfer latency under 5 ms at 50,000 transfers/s on a three-node cluster, and single-node throughput within 2x of TigerBeetle on the same hardware. It is re-run in weeks 20, 33, 47 and 52, and a CI gate fails any change that regresses throughput, p99 or RSS by more than 5%.

## 2026-11 · Core ledger complete: recovery, queries, transfers, feed, reconciliation

The five v1 core themes land at one week each: snapshot recovery and segmented storage, listings and account lifecycle, batches, holds and multi-currency, a change feed with webhooks, and reconciliation v2.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 1 | 2026-11-02 to 2026-11-08 | Fast recovery and storage safety | Startup replays every entry ever written, the log is one growing file, and the only proof a data directory is sound is to start the server. This week turns the whole v1 storage month into snapshot recovery, segments, offline verification, non-blocking invariants and power-loss tests. Closes comparison matrix rows C7, E2. |
| 2 | 2026-11-09 to 2026-11-15 | Listings, indexes and account lifecycle | Clients can fetch an account or transfer only by ID, accounts cannot be frozen or closed, and limits are only zero. This week ships the whole v1 lifecycle month: listings, secondary indexes, freeze and close, and amount limits. Closes comparison matrix rows A9, A14, A3. |
| 3 | 2026-11-16 to 2026-11-22 | Batches, balancing transfers, holds and multi-currency | Marketplace payouts need several transfers to happen together, sweeps fail on short balances, holds cannot grow, and a currency conversion is two separate entries that a crash can split. This week delivers the v1 richer-transfers month. Closes comparison matrix rows A2, A3, A4, A10. |
| 4 | 2026-11-23 to 2026-11-29 | Change feed, live stream, webhooks and export | Downstream systems can only poll accounts to learn what changed. This week makes every journal entry consumable: a paged feed, a live stream, signed webhooks and a streaming export, the v1 integrations month. Closes comparison matrix rows B1, B2, B3, B9. |
| 5 | 2026-11-30 to 2026-12-06 | Reconciliation v2 | Reconciliation results disappear after the response, payout batches produce false breaks, fees and rounding look like errors, and banks send camt.053 instead of CSV. This week delivers the v1 reconciliation month. Closes comparison matrix rows B4, B5. |

## 2026-12 · Access control, leader benchmark, replicas and backups

Scoped keys and a tamper-evident log, the first published benchmark against TigerBeetle and Formance with a CI gate, followers with fenced promotion, and online backups with point-in-time restore and tracing.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 6 | 2026-12-07 to 2026-12-13 | Access control and tamper evidence | Every API key can do everything, rotating keys needs a restart, checksums do not detect a careful edit and one client can fill the writer queue. This week delivers the v1 access-control month. Closes comparison matrix rows D2, D4, D10. |
| 7 | 2026-12-14 to 2026-12-20 | Performance baseline and the leader benchmark | Numbers today measure the writer in-process. This week delivers the v1 performance month and turns it into a published head-to-head benchmark against TigerBeetle and Formance with a CI gate, so every later week is held to measured targets. Closes comparison matrix rows C6, E1, E2, E3, E4. |
| 8 | 2026-12-21 to 2026-12-27 | Read replicas and fenced failover | Reads, statements and reports all load the single writer, and losing the machine means losing service. This week delivers the v1 replica month: log shipping, follower mode, read-your-writes and fenced manual promotion. Closes comparison matrix rows C1. |
| 9 | 2026-12-28 to 2027-01-03 | Backups, point-in-time restore and observability | Copying a directory under a running server is unsafe, there is no way back to a known point, and a slow write cannot be attributed. This week delivers the v1 operations month and is the year's one observability week. Closes comparison matrix rows C4, D13. |

## 2027-01 · Client tools, accounting, 1.0 and a disk engine

OpenAPI, a Go client and CLI, a chart of accounts with period close and statements, the 1.0 single-node release, and the start of storage larger than memory with a copy-on-write B+tree.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 10 | 2027-01-04 to 2027-01-10 | Client ecosystem: OpenAPI, Go client, CLI and history checker | Integrators hand-write HTTP calls and retries, operators compose JSON in a shell, and the model test runs only in-process. This week delivers the v1 client month. Closes comparison matrix rows D7, C7. |
| 11 | 2027-01-11 to 2027-01-17 | Accounting: chart of accounts, periods and statements | Accountants think in typed accounts, roll-ups and closed periods, not flat IDs. This week delivers the v1 accounting month. Closes comparison matrix rows A7, A8, A12, A13. |
| 12 | 2027-01-18 to 2027-01-24 | ledgerd 1.0: nightly fuzzing, signed releases, format guarantees | A 1.0 tells users the single-node API and storage formats will not break under them. This week delivers the v1 release month and is the year's one release engineering and CI week; the benchmark gate already landed in week 7. Closes comparison matrix rows D14. |
| 13 | 2027-01-25 to 2027-01-31 | Copy-on-write B+tree storage engine | Every account, transfer and history row lives in memory, so ledger size is capped by RAM while TigerBeetle and Formance both store state on disk. A page-based copy-on-write B+tree in the standard library is the foundation for larger-than-memory state, MVCC and fast snapshots. Closes comparison matrix rows C3. |

## 2027-02 · Larger than memory and consensus

State moves onto the B+tree with a bounded page cache, reads use MVCC snapshots, and a Raft core with deterministic simulation carries the ledger to quorum commit and automatic failover.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 14 | 2027-02-01 to 2027-02-07 | Ledger state on disk with a bounded page cache | With the engine in place the ledger itself has to move: accounts, transfers, idempotency keys and history onto the B+tree, with a fixed memory budget, so a 1 billion entry ledger runs on a small machine. Closes comparison matrix rows C3, E3. |
| 15 | 2027-02-08 to 2027-02-14 | MVCC snapshots for reads and reports | Long reads (statements, reports, exports, reconciliation) contend with the writer and see a moving ledger. Snapshot isolation on the copy-on-write tree gives every read a consistent version at zero cost to commits. Closes comparison matrix rows A7, C3. |
| 16 | 2027-02-15 to 2027-02-21 | Raft consensus core with deterministic simulation | Fenced manual promotion still needs a human and loses availability; TigerBeetle replicates every batch to a quorum and fails over on its own. A Raft core written for ledgerd, tested by deterministic simulation from the first line, is the basis of the cluster. Closes comparison matrix rows C1, C7. |
| 17 | 2027-02-22 to 2027-02-28 | Ledger on Raft: quorum commit and automatic failover | The core has to carry the ledger: each group-commit batch becomes one Raft entry, acknowledged only after a quorum persists it, and a new leader takes over in seconds without a human. Closes comparison matrix rows C1. |

## 2027-03 · Cluster operations, verification and sharding

Membership changes and rolling upgrades, whole-cluster deterministic simulation, a black-box linearizability campaign, multi-ledger tenancy, and the shard map with a router.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 18 | 2027-03-01 to 2027-03-07 | Membership changes, snapshot install and rolling upgrades | A cluster that cannot add, remove or replace nodes, or upgrade without downtime, is not operable. This week adds the operations every production consensus system needs. Closes comparison matrix rows C1, D8. |
| 19 | 2027-03-08 to 2027-03-14 | Whole-cluster deterministic simulation testing | Unit-level Raft simulation does not cover the ledger, the transport, recovery and timers together. Running the entire cluster in one deterministic process, as TigerBeetle's VOPR does, finds the bugs that only appear in combinations of faults. Closes comparison matrix rows C7. |
| 20 | 2027-03-15 to 2027-03-21 | Black-box linearizability campaign | Simulation proves the code; a black-box test proves the deployed binaries. A Jepsen-style campaign over real processes and networks, with a linearizability checker, backs the consistency claim, and this week carries the second leader benchmark run. Closes comparison matrix rows C7, C6. |
| 21 | 2027-03-22 to 2027-03-28 | Multi-ledger tenancy | Formance runs many isolated ledgers in one deployment; ledgerd has one. Tenancy has to be in place before sharding so every key, quota and route knows its ledger. Closes comparison matrix rows D1, D10. |
| 22 | 2027-03-29 to 2027-04-04 | Shard map, placement and router | One Raft group's throughput is bounded by one leader. Sharding ledgers and account ranges across many Raft groups is how the cluster scales out, starting with a placement service and a router. Closes comparison matrix rows C2. |

## 2027-04 · Cross-shard transfers, rebalancing and a transaction language

Atomic cross-shard transfers, online shard splits, striped hot accounts, and a Numscript-class transaction language.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 23 | 2027-04-05 to 2027-04-11 | Atomic cross-shard transfers | A transfer between accounts in different shards must still be all or nothing. A coordinator log with two-phase commit over the existing pending-transfer model makes it atomic, and in-doubt transfers must resolve after any crash. Closes comparison matrix rows C2, A10. |
| 24 | 2027-04-12 to 2027-04-18 | Online shard splits and rebalancing | Load is never even. Splitting a hot shard and moving account ranges between groups online, without stopping writes, keeps the cluster balanced as it grows. Closes comparison matrix rows C2. |
| 25 | 2027-04-19 to 2027-04-25 | Hot accounts and the second-quarter leader benchmark | Every payment touches the same fee and settlement accounts, so one account becomes the bottleneck in any sharded ledger. Striping hot accounts into sub-balances while keeping limits exact removes it, and the quarter closes with a benchmark re-run. Closes comparison matrix rows C2, C6. |
| 26 | 2027-04-26 to 2027-05-02 | A transaction language for complex money movements | Formance's Numscript lets a payment describe sources, destinations, splits and overdraft rules in one script. ledgerd needs an equivalent so complex flows are one atomic, auditable command instead of client code. Closes comparison matrix rows A11. |

## 2027-05 · Enterprise security and product layer

RBAC with operator SSO, maker-checker approvals, encryption at rest and mTLS, a Merkle-proof audit log, and a wallets layer.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 27 | 2027-05-03 to 2027-05-09 | RBAC and operator single sign-on | Scoped API keys are not enough for a finance team: enterprises need roles, groups and sign-in through their identity provider for every human who touches the ledger. Closes comparison matrix rows D2. |
| 28 | 2027-05-10 to 2027-05-16 | Maker-checker approvals and transfer policies | High-value transfers and manual adjustments need a second person. Policies that hold such transfers for approval, recorded in the log, are a standard control in enterprise ledgers. Closes comparison matrix rows D3. |
| 29 | 2027-05-17 to 2027-05-23 | Encryption at rest and in transit | Financial data must be encrypted on disk and between nodes, with keys that can rotate. Enterprises also expect a key hierarchy that can hand the master key to an external key service. Closes comparison matrix rows D5. |
| 30 | 2027-05-24 to 2027-05-30 | Verifiable audit log with Merkle proofs | A hash chain proves the log was not edited, but an auditor still has to read all of it. A Merkle tree over the journal with signed checkpoints gives inclusion proofs for any entry and consistency proofs between checkpoints, the standard of transparency logs. Closes comparison matrix rows D4. |
| 31 | 2027-05-31 to 2027-06-06 | Wallets layer | Formance Wallets give products balances with buckets, holds and expiring credits on top of the ledger. ledgerd should offer the same product layer so teams do not rebuild it. Closes comparison matrix rows B8. |

## 2027-06 · Money movement: connectors, flows, reconciliation engine, FX

Payment connectors, durable payout flows, a rule-based reconciliation engine with bank formats, and FX revaluation.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 32 | 2027-06-07 to 2027-06-13 | Payment connectors framework | Formance Payments pulls payins, payouts and balances from processors and banks into one model. A connector framework in ledgerd feeds reconciliation and flows with real data. Closes comparison matrix rows B6. |
| 33 | 2027-06-14 to 2027-06-20 | Durable payout flows | Payouts are multi-step: reserve, call a processor, wait, confirm or compensate. Formance Flows orchestrates this; ledgerd needs a durable workflow engine on its own log so money movements never strand. Closes comparison matrix rows B7. |
| 34 | 2027-06-21 to 2027-06-27 | Rule-based reconciliation engine and bank formats | Real reconciliation needs matching rules finance teams can change, more bank formats, and a workflow to resolve breaks with adjustments. This takes reconciliation from a matcher to an engine, and carries the third quarterly benchmark. Closes comparison matrix rows B4, B5, C6. |
| 35 | 2027-06-28 to 2027-07-04 | FX rates and revaluation | Multi-currency books need rate sources, period-end revaluation and realized versus unrealized gains. Without them a treasury team cannot close the books. Closes comparison matrix rows A4, A5. |

## 2027-07 · Web console and SDKs

A web console for operations and reconciliation, and TypeScript and Python SDKs held to one contract suite.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 36 | 2027-07-05 to 2027-07-11 | Web console: ledgers, accounts and transfers | Formance ships a console; finance and support teams will not use curl. A server-rendered console embedded in the binary keeps the footprint small and needs no JavaScript framework. Closes comparison matrix rows D6. |
| 37 | 2027-07-12 to 2027-07-18 | Web console: reconciliation, approvals and audit | The workflows that need humans most are reconciliation breaks, approvals and audits. The console second half brings them to the browser. Closes comparison matrix rows D6, B4. |
| 38 | 2027-07-19 to 2027-07-25 | TypeScript SDK | TigerBeetle and Formance both ship clients for the major languages. A zero-dependency TypeScript SDK on fetch serves Node, Deno, Bun and edge runtimes. Closes comparison matrix rows D7. |
| 39 | 2027-07-26 to 2027-08-01 | Python SDK | Finance and data teams work in Python. A standard-library-only client keeps installation trivial and passes the same conformance suite. Closes comparison matrix rows D7. |

## 2027-08 · Wire protocol, data pipelines and lifecycle

A Java SDK, a binary batch protocol, change data capture to brokers, Parquet export, and archival with metadata redaction.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 40 | 2027-08-02 to 2027-08-08 | Java SDK | Banks and large payment teams run on the JVM, and TigerBeetle's Java client is one of its most used. A Java SDK on java.net.http with no dependencies completes the language set. Closes comparison matrix rows D7. |
| 41 | 2027-08-09 to 2027-08-15 | Binary batch protocol | JSON over HTTP caps throughput well below what the engine can do; TigerBeetle uses fixed-size binary records over TCP. A binary batch protocol beside the HTTP API lets high-volume clients reach engine speed. Closes comparison matrix rows E4, C6. |
| 42 | 2027-08-16 to 2027-08-22 | Change data capture to message brokers | Enterprises move ledger events into their message buses. Native producers for AMQP 0-9-1 and the Kafka protocol, written on the standard library, avoid a sidecar. Closes comparison matrix rows B1. |
| 43 | 2027-08-23 to 2027-08-29 | Analytics export in Parquet | Data warehouses want columnar files. A Parquet writer on the standard library and incremental exports by period let analysts query the ledger without touching it. Closes comparison matrix rows B9. |
| 44 | 2027-08-30 to 2027-09-05 | Archival, retention and metadata redaction | Ledgers keep entries forever, but storage tiers and privacy law do not. Cold history moves to cheaper storage, and personal data in metadata can be erased without breaking the journal. Closes comparison matrix rows D12. |

## 2027-09 · Deployment, multi-region and performance

Kubernetes deployment, an asynchronous cross-region replica, a faster write path, and lower memory and cold start, measured against the leaders.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 45 | 2027-09-06 to 2027-09-12 | Kubernetes deployment | Every self-hosted competitor in the matrix ships a supported Kubernetes path, and ledgerd's cluster needs ordered bootstrap, quorum-aware disruption rules and safe rolling upgrades. This is the year's one packaging week. Closes comparison matrix rows D8. |
| 46 | 2027-09-13 to 2027-09-19 | Cross-region standby and disaster recovery | A cluster in one region does not survive a regional outage, and the hosted ledgers in the matrix advertise multi-region durability. An asynchronous standby cluster with measured RPO and RTO and a tested failover and failback closes that row. Closes comparison matrix rows C5. |
| 47 | 2027-09-20 to 2027-09-26 | Faster write path and the third-quarter leader benchmark | Features from the last six months added work to every commit. This week tunes the whole write path against the published targets (matrix rows E3 and E4) and re-runs the quarterly benchmark against TigerBeetle and Formance. Closes comparison matrix rows E4, C6. |
| 48 | 2027-09-27 to 2027-10-03 | Lower memory and faster cold start | Footprint is where a single static Go binary can beat every competitor (matrix rows E1 to E3). This week drives resident memory, cold start and image size to the published targets and makes them CI budgets. Closes comparison matrix rows E1, E2, E3. |

## 2027-10 · Migration, soak, hardening and the cluster GA

Importers from other ledgers, a chaos and soak campaign, a security hardening pass, and ledgerd 2.0 with the final leader benchmark.

| Week | Dates | Deliverable | Why |
|---|---|---|---|
| 49 | 2027-10-04 to 2027-10-10 | Migration from other ledgers | Teams only adopt a ledger if they can move their books in and prove nothing changed. Importers from TigerBeetle, Formance and Modern Treasury-style exports plus CSV journals, and a dual-run comparator, close matrix row D9. Closes comparison matrix rows D9. |
| 50 | 2027-10-11 to 2027-10-17 | Chaos and soak campaign | Hosted ledgers publish uptime; a self-hosted ledger has to show evidence. A 72-hour soak on a five-node cluster under continuous faults backs the production-readiness claim in matrix row C1. Closes comparison matrix rows C7, C1. |
| 51 | 2027-10-18 to 2027-10-24 | Security hardening and compliance evidence | Hosted competitors sell SOC 2 and PCI DSS reports; a self-hosted ledger cannot be certified itself but can make its operators' audits easy (matrix row D11). This week hardens the full surface and ships a controls mapping with evidence the system produces. Closes comparison matrix rows D11. |
| 52 | 2027-10-25 to 2027-10-31 | ledgerd 2.0: cluster GA and the final leader benchmark | The year ends with a supported cluster release that 1.x users can upgrade to, a final benchmark against every leader that publishes numbers, and the comparison matrix updated to what shipped. Closes comparison matrix rows C6, D14. |

## Next year

Gaps from the comparison matrix that stay open after October 2027:

- Bank rail origination (ACH, wire, instant payments) as in Stripe Treasury: needs banking partners (rows B6).
- Certified SOC 2 and PCI DSS reports belong to whoever hosts ledgerd; the roadmap ships only the evidence pack (row D11).
- SDKs for .NET, Ruby and Rust to match TigerBeetle's seven client languages (row D7).
- Request hedging and cross-replica storage-fault repair at TigerBeetle's depth (rows C1, C7).
