# cex-lab

A small learning lab for the core of a centralized crypto exchange (CEX):
a double-entry ledger, a spot matching engine, the glue between them, and a
deposit watcher that copes with chain reorganisations.

**This is a learning project, not production software.** It moves no real
funds, talks to no real blockchain, has no network layer or authentication,
and has not been audited. I built it to work through the hard parts of an
exchange with code that tests can check. I have no production
matching-engine or EVM experience; this repo is how I am learning them.

Go standard library only, no third-party modules.

## Run

```sh
make check       # gofmt, go vet, go test -race ./...
make demo        # go run ./cmd/demo (serves /metrics on 127.0.0.1:<random port>)
make demo-serve  # same on 127.0.0.1:9090 and keeps serving 60s for curl
make fuzz        # 30s of go test -fuzz on random command sequences
make bench       # metrics overhead benchmarks
```

## What is in it

| Package | What it does |
|---|---|
| `ledger` | Double-entry, append-only ledger. Accounts per (user, asset, available/locked) plus system accounts (fees, external, withdrawals in flight). Deposit, Hold, Release, Settle (one trade, with fees and refund), Withdraw request/confirm/fail. Every command has an idempotency key. In-memory store or an fsynced JSON-lines journal; state is rebuilt by replay. |
| `outbox` | Relay that drains ledger events at least once, and a consumer inbox that deduplicates by event id. |
| `engine` | Spot matching engine for one market: limit and market orders, price-time priority, partial fills, cancel, tick/lot validation. One writer goroutine per market, sequenced command log written before applying, deterministic replay. |
| `exchange` | Glue: place = hold, match, settle each trade, release leftovers. Cancel = release. `Check()` asserts cross-component invariants. |
| `chain` | `Client` interface, a deterministic fake chain with reorgs, and a deposit watcher with confirmations, reorg handling, idempotent crediting and a persisted cursor. |
| `internal/journal` | Append-only fsynced JSON-lines file, shared by the ledger journal and the engine command log. |
| `telemetry` | Hand-written metrics registry (counter, gauge, histogram, labels), Prometheus text exposition, `/metrics`, `/healthz`, `/readyz`. |
| `telemetry/trace` | Trace id carried in `context.Context`. |
| `instrument` | Adapters from each package's `Metrics` interface to the registry. |
| `cmd/demo` | Runs the whole path once and prints trades, balances and the invariant check. |

## Architecture

```
            chain.Client (fake chain in tests)
                     │ blocks, transfers
                     ▼
              chain.Watcher ── N confirmations, reorg detection,
                     │          cursor saved atomically
                     │ Deposit{key = chain:txhash:logindex}
                     ▼
 Place/Cancel ┌────────────────────────────────────────────┐
 ───────────► │ exchange (per-market mutex)                │
              │  1. ledger.Hold (available → locked)       │
              │  2. engine.Submit ───────────┐             │
              │  3. ledger.Settle per trade  │             │
              │  4. ledger.Release leftover  │             │
              └──────┬───────────────────────┼─────────────┘
                     │                       ▼
                     │          engine (one goroutine per market)
                     │           seq++ → command log (fsync) → Book.Apply
                     ▼
              ledger (mutex) ── Tx{entries, event} → journal (fsync) → memory
                     │
                     ▼ EventsSince(offset)
              outbox.Relay ──at-least-once──► outbox.Inbox (dedup by event id)
```

Money flows between accounts as follows (every row is one balanced transaction):

| Command | Entries |
|---|---|
| Deposit | external −x, user.available +x |
| Hold | user.available −x, user.locked +x |
| Release | user.locked −x, user.available +x |
| Settle | buyer.locked(quote) −(q+refund), buyer.available(quote) +refund, buyer.available(base) +(b−fee), fees(base) +fee, seller.locked(base) −b, seller.available(quote) +(q−fee), fees(quote) +fee |
| WithdrawRequest | user.available −x, withdrawals_in_flight +x |
| WithdrawConfirm | withdrawals_in_flight −x, external +x |
| WithdrawFail | withdrawals_in_flight −x, user.available +x |

`external` mirrors money outside the exchange, so it is the only account
allowed to be negative; `−external` is the supply of an asset on the
exchange. There is no `deposits_in_flight` account: deposits that are not yet
confirmed live only in the watcher's pending set and are not on the ledger
at all.

## Invariants

Checked after every step of the random simulation
(`exchange/property_test.go`), and partly by `ledger.Verify` and
`exchange.Check`:

1. No negative available or locked balance (no account except `external` is negative).
2. For each asset, the sum of all entries is zero.
3. Supply of each asset equals deposits minus confirmed withdrawals,
   tracked by an independent model in the test.
4. Locked balance of every (user, asset) equals the reserve of that user's
   resting orders (buy: price × remaining lots; sell: remaining quantity).
5. `withdrawals_in_flight` equals the sum of pending withdrawals.
6. The book is never crossed (best bid < best ask).
7. Rebuilding the ledger from its journal and each book from its command log
   gives exactly the live state.
8. Retrying an already committed command returns the stored result and
   changes no balance and emits no event.

Chain watcher properties tested in `chain/watcher_test.go`: credit only after
N confirmations; reorg before confirmation drops the pending deposit and
re-adds it if re-included; reorg that orphans a credited deposit returns a
`ReorgAlert` and halts without changing balances; duplicate delivery credits
once; resume from a persisted cursor, including a reorg that happened while
the watcher was down; crash after credit but before saving the cursor does not
double credit.

## Tests

66 test functions, 1 fuzz target (`FuzzExchange`) and 3 benchmarks; tests run with `-race`:

- unit tests per package (ledger commands, idempotency, journal torn-write
  handling, matching rules, validation, cancel, fees, market orders, restart);
- `TestPropertyRandomSequences`: `testing/quick` drives 100 seeds × 300 random
  deposit/place/cancel/withdraw/confirm/fail/retry steps and checks the
  invariants after each step;
- `FuzzExchange`: the same simulation driven by fuzzer bytes;
- `TestReplayIsDeterministic`: random command streams, then replay the log
  and compare trades, results and book;
- concurrency tests: many goroutines submitting to one engine, and many
  goroutines trading on the exchange, then invariants and replay;
- observability tests (`telemetry/`, `instrument/`): the Prometheus output is
  parsed back (HELP/TYPE, escaping, cumulative buckets, `+Inf`, `_sum`,
  `_count`), concurrent updates under `-race`, `/metrics` `/healthz` `/readyz`
  via `httptest`, and scenarios that assert exact metric values (one client
  retry → exactly one `idempotent_replay`; a rejected order →
  `rejected{reason}`; a reorg → `chain_reorgs_total` and depth histogram; an
  injected invariant violation → `invariant_ok 0`, a violation count and a
  503 from `/readyz`; one trace id visible in exchange, engine, ledger and
  outbox logs).

I also checked that the property test catches a real bug: dropping the
price-improvement refund in settlement makes it fail with
`alice/USDT/locked = 797, resting orders reserve 762`.

## Design decisions and trade-offs

- **Integer money.** All amounts are `int64` minimal units. Prices are quote
  units per lot, so a fill's notional is `price × qty/lot` with no rounding.
  Fees are `floor(amount × bps / 10000)` computed with 128-bit intermediates.
  Deposits are bounded so no balance sum can overflow. Trade-off: `int64`
  cannot hold 18-decimal token amounts above ~9.2 tokens; a real system
  would use `big.Int` or a scaled unit.
- **Single-writer engine.** Each market's book is owned by one goroutine fed
  by a channel. No locks inside matching, and a total order of commands for
  free. The book itself (`engine.Book`) is a pure state machine with no
  clock or randomness.
- **Log before apply, deterministic replay.** The engine assigns the next
  sequence number and appends the command to the log before applying it.
  Rejected commands are logged too, so replay reproduces them. Recovery is
  "replay the log"; there are no snapshots yet.
- **Ledger durability.** A transaction is appended and fsynced before memory
  changes. On load, a torn trailing record (never acknowledged) is truncated;
  a corrupt record in the middle stops startup.
- **Idempotency.** Every ledger command has a key. Same key and same payload
  returns the stored transaction; same key with a different payload is an
  error. Rejected commands (for example insufficient funds) are not stored,
  so a retry after funding is evaluated again. The exchange derives keys from
  order ids and trade sequence numbers, so internal retries are safe too.
- **Exchange pipeline is serialised per market.** Hold → match → settle runs
  under a per-market mutex, so ledger reserves and the book agree whenever
  no call is in flight, and settlement of one order finishes before the next
  order matches. It costs throughput; a production design would settle
  asynchronously from the engine's trade stream and accept that reserves and
  the book are briefly out of step.
- **Outbox in the same transaction.** Each ledger transaction stores its
  event, so there is no commit without an event and no event without a
  commit. The relay is at-least-once; consumers deduplicate by event id.
- **Reorgs.** The watcher keeps a window of recent block hashes. A reorg that
  only touches pending deposits is handled automatically. A reorg deeper
  than the confirmation threshold is treated as an incident: alert and halt,
  never a silent reversal of credited funds.

## Mapping to concepts

| Concept | Where |
|---|---|
| Idempotency | `ledger.Ledger.Apply` (key + payload fingerprint); chain credit key `chain:txhash:logindex`; exchange keys `hold:<order>`, `settle:<market>:<seq>:<i>`, `release:<order>:<reason>` |
| Transactional outbox | `ledger.Tx.Events` stored with the entries; `outbox.Relay` drains from an offset |
| Inbox / dedup | `outbox.Inbox` |
| Event sourcing | engine command log is the source of truth for the book; ledger journal is the source of truth for balances; both rebuilt by replay |
| Event-driven | ledger events relayed to consumers at least once |
| Durability | fsync on every journal and command-log append; atomic temp-file + rename for the watcher cursor; torn-write handling |
| Observability | per-package `Metrics` interfaces with no-op defaults, `instrument/` adapters, Prometheus exposition, `log/slog` JSON logs with a trace id from API call to outbox event |

## Observability

Every package that does work (`ledger`, `internal/journal`, `engine`,
`exchange`, `outbox`, `chain`) defines a small `Metrics` interface and takes
it as an option, defaulting to a no-op. There is no global registry, so tests
stay isolated. `instrument/` maps those interfaces onto a
`telemetry.Registry`. Loggers are injected the same way (`*slog.Logger`,
discard by default; JSON in the demo).

### Metrics

| Area | Metrics |
|---|---|
| exchange | `exchange_orders_placed_total{market,side,type}`, `exchange_orders_rejected_total{market,reason}`, `exchange_orders_cancelled_total{market}`, `exchange_trades_total{market}`, `exchange_trade_volume_total{market,asset}` (minimal units), `exchange_order_to_trade_latency_seconds{market}` (order received → first fill returned by the engine), `exchange_place_order_duration_seconds{market}` |
| engine | `engine_command_queue_depth{market}`, `engine_commands_applied_total{market,type}`, `engine_match_duration_seconds{market}`, `engine_book_depth{market,side}` (price levels), `engine_last_seq{market}` |
| ledger | `ledger_commands_total{type,result}` with result `applied`, `idempotent_replay`, `rejected` or `error`; `ledger_idempotency_conflicts_total`, `ledger_entries_written_total` |
| journal | `journal_fsync_duration_seconds{journal}`, `journal_bytes{journal}`, `journal_truncated_records_total{journal}` (torn records dropped on recovery) |
| outbox | `outbox_backlog`, `outbox_oldest_age_seconds`, `outbox_relay_publish_total{result}`, `inbox_duplicates_total` |
| invariants | `invariant_check_runs_total`, `invariant_violations_total{invariant}`, `last_invariant_check_timestamp_seconds`, `invariant_ok` |
| chain | `chain_head_block`, `chain_confirmed_block`, `chain_watcher_lag_blocks`, `chain_deposits_credited_total{asset}`, `chain_deposits_pending`, `chain_reorgs_total`, `chain_reorg_depth_blocks`, `chain_reorg_below_confirmation_threshold_total`, `chain_duplicate_deliveries_total` |

Invariant names come from `ledger.InvariantError`: `entries_sum_zero`,
`balance_matches_entries`, `non_negative_balance`,
`in_flight_matches_pending`, `book_not_crossed`, `locked_equals_reserves`.

Cardinality rules: labels are only market, side, order type, asset, command
type, result, reject reason (a fixed list), invariant name and journal name.
User ids, order ids, tx hashes and idempotency keys go to logs, never to
labels. An unknown market symbol is counted as `market="unknown"` so a
client cannot create series.

### What I would alert on

| Signal | Why |
|---|---|
| `increase(invariant_violations_total[5m]) > 0` or `invariant_ok == 0` | Balances and the book disagree. Page immediately and stop trading (the node also goes unready). |
| `increase(chain_reorg_below_confirmation_threshold_total[1h]) > 0` | Funds were credited on blocks that no longer exist. Page; the watcher halts by design. |
| `time() - last_invariant_check_timestamp_seconds` too large | The checker itself stopped, so the first alert would not fire. |
| `outbox_backlog` or `outbox_oldest_age_seconds` growing | Commits succeed but consumers (notifications, risk, market data) do not see them. |
| `engine_command_queue_depth` growing while `rate(engine_commands_applied_total)` is flat | The market's single writer is stuck or too slow. |
| p99 of `journal_fsync_duration_seconds` | Disk latency is the floor for every order and every ledger write. |
| spike in `ledger_idempotency_conflicts_total` | A client or internal caller reuses keys for different payloads, which is a bug. |
| `chain_watcher_lag_blocks` above a few blocks, or `chain_deposits_pending` growing | Deposits are slow or the node is unhealthy. |
| `rate(exchange_orders_rejected_total{reason="internal"})` > 0 | Errors that are not user mistakes. |

### Dashboards I would build

1. Trading: orders placed/rejected by reason, trades and volume per market,
   p50/p99 place and order-to-trade latency, book depth.
2. Engine health: queue depth, applied commands rate, match latency, last seq
   per market (a flat line on a busy market is a stuck writer).
3. Ledger and durability: command results stacked by type, replay and
   conflict rates, fsync latency heatmap, journal size.
4. Money safety: `invariant_ok`, violations by invariant, time since last
   check, outbox backlog and age.
5. Deposits: head vs confirmed block, watcher lag, pending deposits, reorgs
   and depth histogram, credits per asset.

### Logs and trace correlation

`exchange.Place` takes the trace id from the context or creates one
(`telemetry/trace`). It travels in `engine.Command.TraceID` (written to the
command log, never used by matching), in `ledger.Tx.TraceID`, and in
`ledger.Event.TraceID`, so the outbox relay logs it too. The demo prints the
JSON lines of one order: hold → engine command → two settlements → order
placed → three outbox events, all with the same `trace_id`. Log fields
include `market`, `seq`, `order_id`, `user_id`, `idempotency_key`,
`event_id`, `tx_key`. There are no secrets in this project to leak; logs
carry no keys or credentials.

### Readiness

`/healthz` answers 200 while the process serves HTTP. `/readyz` answers 503
if the last invariant check failed or the last journal or command-log
append failed (`FileStore.Err`, `FileLog.Err`). It reports the most recent
results; it does not probe the disk itself.

### Overhead

`make bench` on my machine: `Counter.Inc` about 60 ns, `Histogram.Observe`
about 25 ns; `ledger.Apply` was about 1.4 µs/op with the no-op recorder and
about the same with the real one (the difference was inside run-to-run
noise). Each update takes a per-family mutex, which is simple and fine for
this lab but would be contended under heavy parallel load.

### Not done

- No OpenTelemetry: no OTel SDK, no OTLP exporter, no spans. The trace id is
  a log-correlation id only. OTel tracing and metrics export is the next step.
- I did not run Prometheus or Grafana against it. The exposition format is
  checked by tests that parse it, not by a real scraper.
- Metric values are float64 like Prometheus, so volume counters lose
  precision above 2^53 minimal units.
- No alert rules or dashboards are committed; the lists above are design.

## Limitations

- No network API, WebSocket feed, authentication, rate limits or admin tools.
- No real blockchain. `anvil`/`geth` were not available where this was built,
  so there is no real EVM adapter; the watcher runs only against the fake
  chain.
- No signing, key management or withdrawal broadcasting. Withdraw confirm/fail
  are plain ledger commands.
- In-memory plus local files only; no database, replication or failover.
- No snapshots, so recovery time grows with the log.
- Self-trade is not prevented.
- Observability is in-process only: no OpenTelemetry, and no real
  Prometheus/Grafana has been run against it.
- Not audited, not load-tested, not reviewed by anyone else.

## What I would do next

1. WebSocket market data (book deltas by sequence number, snapshot + replay
   on reconnect).
2. Postgres-backed ledger with the outbox and inbox tables in the same
   transaction as the entries.
3. Engine snapshots plus log truncation; settle asynchronously from the
   trade stream.
4. A real EVM adapter (go-ethereum `ethclient`, ERC-20 `Transfer` logs)
   behind a build tag, tested against a local anvil node.
5. Signer isolation for withdrawals: a separate service with its own keys
   and limits, never in the exchange process.
6. Self-trade prevention, order types (IOC, FOK, post-only), per-user limits.
7. OpenTelemetry tracing (spans for place → match → settle → publish) and
   OTLP metric export; alert rules and dashboards as code.
8. External review of the ledger and settlement logic.
