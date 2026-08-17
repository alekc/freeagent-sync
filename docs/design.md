# freeagent-sync: design plan

Design document for this repository. Revision 2.


## 1. Objective

Own a complete, local, browsable copy of a FreeAgent company that somebody else administers. The
accountant drives the live account; this is the reader that makes the data mine. Bank
transactions, invoices, bills, expenses, the scanned receipts attached to them, and the ledger
behind them.

Ranked goals:

1. **Completeness.** Nothing the API exposes is silently dropped, including fields the SDK does
   not model and fields FreeAgent has not documented.
2. **Fidelity.** Money stays exact. Deletions are noticed. Every change is attributable to a
   point in time, and history is kept forever.
3. **Portability.** The output is a plain SQLite file, a directory of files, and a browsable tree
   of names pointing into it. Readable in ten years by anything, with or without this tool.
4. **Safety.** The tool cannot write to the live company. Not "does not"; cannot.
5. **Legibility.** An interactive pull shows what it is doing. A cron pull says what it did.

Out of scope for this build, but the schema reserves room for it: writing into FreeAgent, and
keeping two companies in step. See section 17.

## 2. What the SDK provides

Verified against the checkout, not assumed:

| Need | Provided by |
| --- | --- |
| Auth with refresh-token rotation | `TokenSource` plus `FileStore`, keyed by an arbitrary string, so one file holds many accounts |
| Rate limiting under the published caps | client-side limiter at 100/min and 3400/hour, plus `Retry-After` aware retry |
| Generic enumeration of any family | `Client.Raw(ctx, GET, path, query, nil)` returns the undecoded body plus `*Response` |
| Page walking | `Response.FirstPage/PrevPage/NextPage/LastPage`, parsed from the `Link` header |
| Family metadata | `freeagent.Resources`, with `Plural`, `Singleton`, `ReadOnly`, `NoList`, `Grouped`, `RequiresBankAccount`, `CustomEnvelope` |
| Incremental filter and date ranges | `ListOptions.UpdatedSince`, `FromDate`, `ToDate`; `MaxPerPage` is 100 |
| Write refusal | `WithReadOnly()`, enforced in `Client.newRequest`, so no service and no `Raw` call can escape it |
| Exact money | `Decimal` (shopspring) on every monetary field |
| Reports for cross-checking | `Reports.TrialBalance`, `ProfitAndLoss`, `BalanceSheet`, `Cashflow` |

### Three families the SDK does not model

Comparing the API's documented resource index against `freeagent.Resources` found three with no
registry entry. All three are reachable through `Raw` today, which is the first dividend of the
raw-first design in section 4. Each is worth a separate SDK issue.

- **Account Locks** (`account_locks`, envelope `account_locks`): `locked_to_date`, `user_lock`,
  `locked_by_description`, `locked_by_url`, `earliest_lock_date`, `latest_lock_date`. Directly
  relevant to an accountant-managed company: it says which periods have been closed. Note it
  supports PUT and DELETE, so the read-only guard is doing real work here.
- **Currencies**: needed to know a currency's exponent rather than assuming two decimal places.
- **Accountancy Practice API**: for practices managing client companies, so probably not
  applicable from the client side. Noted, not planned.

## 3. What the API cannot give

Three hard limits that shape everything downstream. None has a workaround.

**The Files area is unreachable.** There is no `files` resource. The Files and Smart Capture
areas hold uploads that have not been attached to anything, and attaching a file to a bill or
expense *removes it from the Files area*, per FreeAgent's own documentation. The two sets are
disjoint: attached files are reachable as `attachment` on bills, expenses and bank transaction
explanations, and everything still sitting in Files is invisible to the API. With 1GB of Files
storage available, unmatched receipts can accumulate there and this tool structurally cannot see
them. The only mitigation is a manual download from the web interface, and `fasync status`
should carry a standing note saying so rather than letting the archive imply completeness it
does not have.

**There is no deletions feed.** `updated_since` never reports a removed record and
`/docs/changes` is a human changelog. Deletion detection requires a periodic full-key sweep.

**There are no webhooks.** Sync is poll-only.

### Where attachments actually live

`Attachment` is a field on exactly three types: bills, expenses, and bank transaction
explanations. Sales invoices, estimates and credit notes have no attachment field, because their
document is generated rather than uploaded. So there are two blob sources with different costs:

- `attachment.content_src` (plus `_medium`, `_small`): a time-limited URL on a third-party host.
  No auth needed, and fetching it does not spend the API rate budget. `expires_at` is on the
  record, so a resumed download may need its metadata re-resolved through `Attachments.GetURL`.
- `Invoices.PDF`, `Estimates.PDF`, `CreditNotes.PDF`: API endpoints, so these do spend budget.

Per-file limit is 5MB, matching the SDK's `MaxAttachmentBytes`.

## 4. Architecture: raw first, everything else derived

**Layer 1, the archive.** Every response body stored verbatim as JSON, keyed by resource URL,
with a version row appended whenever the body changes. This is the only source of truth.

**Layer 2, the derived views.** Typed normalised SQL tables, the JSON record tree on disk, and
the browsable symlink tree. All three are projections, all three rebuildable offline from layer 1
with no network access.

Why that order:

- The SDK found a dozen places where FreeAgent's documentation disagrees with the API, and eight
  fields the API returns that the docs omit. A field nobody has modelled yet is a field a
  typed-only mirror loses forever.
- Re-fetching means going back to somebody else's live production accounting account. The
  archive should need fetching once.
- Schema changes stop being migrations against irreplaceable data and become a reprojection.
- Layer 2 can land family by family without holding up the archive, so the first working version
  already mirrors everything.
- It is also why the three unmodelled families above cost nothing to capture.

The cost is roughly double storage on projected families. Megabytes, for one company.

## 5. Storage and on-disk layout

**SQLite through `modernc.org/sqlite` v1.56.0**, the pure-Go CGo-free driver. Satisfies the
no-preexisting-executables requirement outright: an ordinary Go dependency, a static binary, and
a standard `.sqlite` file that the `sqlite3` CLI, DB Browser, Datasette and DuckDB can all open.
Encryption is out of scope, which is what settles this over `ncruces/go-sqlite3`: that one's
pure-Go encryption VFS was its only real advantage, and an encrypted database readable by one
library contradicts goal 3.

Pragmas at open: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`,
`synchronous=NORMAL`.

```
~/.local/share/freeagent-sync/          (XDG default, --data-dir overrides)
  freeagent.sqlite                      source of truth
  blobs/ab/cd/abcdef...                 content-addressed by sha256, two-level fanout
  tmp/                                  partial downloads, renamed in only once hashed
  records/<family>/<id>.json            derived: current body, pretty, sorted keys
  records/<family>/<id>.versions/<ts>-<sha8>.json
  files/by-date/2026/03/...             derived: symlinks into blobs/
  files/by-family/bills/1234/...
  files/by-contact/acme-ltd/...
  .lock                                 flock, one writer at a time
```

Directory `0700`, DB file `0600`. At-rest protection is delegated to full-disk encryption and
documented as such.

**Blobs are content-addressed files, not BLOB columns.** Free dedupe when the same receipt is
attached twice, free integrity checking, and the database stays small enough to copy and query
cheaply. Downloads land in `tmp/`, are hashed while streaming, and are renamed into place only
once the digest is known, so a killed run leaves no half files.

**The records tree is derived, not written in parallel.** SQLite is the write-ahead source of
truth and `fasync files rebuild` regenerates the tree from it. Writing both during a pull would
let a crash leave them disagreeing, with no way to tell which was right. Pretty-printed with
sorted keys so ordinary diff tools work on the version history.

**The symlink tree is three views over the same blobs**, rebuilt idempotently by
`fasync files relink`, which wipes and recreates it because symlinks are free. Names are
sanitised (separators, colons, control characters, length) and collisions resolved
deterministically by appending the short digest. On filesystems without symlinks, hardlink or
copy, selected by a flag. Case-insensitive filesystems need collision handling on slugs that
differ only by case, which is macOS by default.

**Migrations** are embedded `.sql` files stepped by SQLite's own `user_version`, roughly sixty
lines of Go and no dependency. A `meta` table also carries `schema_version`, checked at open, so
an older binary against a newer database fails loudly rather than querying columns that moved.

**Accounts live in the database**, managed by `fasync account add`, so there is no config file
format to choose and no third surface to keep consistent. Client credentials come from the
environment (`FREEAGENT_CLIENT_ID`, `FREEAGENT_CLIENT_SECRET`), tokens stay in the SDK's `0600`
store keyed per account, paths come from flags. All validated once, at startup, in one place.

## 6. Schema sketch

Every table carries `account_id`. A second company is the stated future direction and this is the
one thing that cannot be retrofitted cheaply.

```sql
meta(key, value)                          -- schema_version, created_at, tool_version

accounts(id, slug, name, environment, company_url, created_at, disabled_at,
         role, writable)                  -- role/writable RESERVED, writable defaults 0

-- Layer 1
records(account_id, family, url, remote_id, body, body_sha256,
        remote_updated_at, first_seen_at, last_seen_at,
        deleted_at, deleted_detected_by_run,
        final_at,                         -- set from account_locks, advisory
        local_body, local_dirty_at, push_state, pushed_at, push_error, source_url)
                                          -- the six local_*/push_*/source_url: RESERVED
record_versions(account_id, url, body_sha256, body, remote_updated_at, seen_at)

-- Layer 2, derived and rebuildable
invoices(...) bills(...) bank_transactions(...) contacts(...) ...
report_snapshots(account_id, report, from_date, to_date, taken_at, body)

-- Blobs
blobs(sha256 PRIMARY KEY, size, content_type, stored_at, verified_at)
attachments(account_id, url, parent_url, family, file_name, content_type,
            file_size, content_src, expires_at, sha256 REFERENCES blobs,
            state, attempts, last_error)
documents(account_id, parent_url, kind, sha256 REFERENCES blobs,
          rendered_at, rendered_for_updated_at)

-- Control
sync_runs(id, account_id, mode, direction, started_at, finished_at, families,
          window_from, window_to, changed_since, changed_until,
          requests, records_upserted, records_deleted, bytes_downloaded,
          outcome, error)                 -- direction RESERVED, always 'pull'
sync_state(account_id, family, scope, cursor_updated_at, last_full_reconcile_at,
           supports_updated_since, last_run_id)
capabilities(account_id, family, probe, result, probed_at)
account_locks(account_id, locked_to_date, user_lock, earliest_lock_date,
              latest_lock_date, observed_at)

-- RESERVED for the write path, created empty
identity_map(source_account_id, source_url, target_account_id, target_url, linked_at)
```

`deleted_at` is a soft delete. Rows are never removed. If the accountant deletes an invoice I
want to keep it and know when it went.

**Money.** SQLite has no decimal type and `REAL` would break the SDK's hardest rule. Every
decimal is stored twice: the canonical exact string in `TEXT`, and a scaled `INTEGER` at a fixed
scale of 6 (`*_e6`) so SQL can `SUM` exactly. A value carrying more than six decimal places
fails the write loudly rather than being rounded silently.

**Reserved columns are created, documented as unused, and never read.** Cheaper than a migration
against an archive later, and a comment in the migration file says which are which.

## 7. Sync engine

### Family classes

Driven off `freeagent.Resources` rather than forty hand-written syncers:

| Class | Detected by | Strategy |
| --- | --- | --- |
| Incremental collection | has `Plural`, no scope flags | `updated_since` cursor, page walk, periodic reconcile |
| Account-scoped, date-ranged | `RequiresBankAccount` | fan out over bank accounts, then date windows |
| Singleton | `Singleton` | snapshot per run, new version only if the body changed |
| Report | `Singleton` and `ReadOnly` under `accounting/` | dated snapshot rows, never upserted |
| Period-addressed | the four filing families | enumerate periods, then fetch each |
| Grouped envelope | `Grouped` | categories only, decode the several keys |
| Child-only | `NoList` | reached from the parent payload, never enumerated |
| Unmodelled | not in the registry | explicit path list: `account_locks`, `currencies` |

Layer 1 only needs raw enumeration, so the archive path is one generic pager over `Raw` plus
`Response.NextPage`. The SDK has no `RawAll` iterator, so this repo carries a thin one; promoting
it upstream should wait for a second consumer.

Families are pulled parents first: contacts, categories, currencies, bank accounts, projects,
then tasks, invoices, bills, expenses, explanations. The archive does not need the ordering, the
projections do, and the write path will need it absolutely.

### Cursors

- Advance the cursor to the **maximum `updated_at` observed in the responses**, never to
  `time.Now()`. A record written while the run was in flight must not be skipped.
- Re-read from `cursor - overlap` (default 1h) next run, absorbing clock skew and any lag between
  a record's `updated_at` and its visibility. Upserts are idempotent, so overlap costs requests
  and nothing else.
- Page-number pagination over a collection being edited can shift a record between pages and skip
  it, and there is no cursor-based pagination to switch to. The mitigation is the overlap plus
  the reconcile. Written down, not papered over.
- **An explicit time window makes the run ad-hoc and must not touch the stored cursor.**
  Otherwise a narrow manual pull leaves the next scheduled run believing it is caught up, and the
  gap surfaces only at the next reconcile. `sync_runs.mode` records which kind of run it was.

### Reconcile

`fasync reconcile` walks every key of every enumerable family with no `updated_since`, stamps
`last_seen_at`, and soft-deletes anything not seen. Also the only source of an exact per-family
count. Weekly by default, or on demand, or via `pull --reconcile-if-due`.

### Capability probe

Nothing records which families honour `updated_since`, and a family that ignores it returns
everything, which looks like success. `fasync probe` settles it in one request per family: ask
with `updated_since` set to a far-future instant, and a non-empty response proves the parameter
is ignored. Result stored in `sync_state.supports_updated_since` rather than guessed.

### Account locks, used advisedly

`locked_to_date` says which periods the accountant has closed. Two uses:

1. Stamp `records.final_at` so a query can distinguish filed from provisional data.
2. Narrow the default re-scan range on date-windowed families.

**Advisory only.** A lock stops transaction edits; assuming it stops every metadata change and
then skipping those periods entirely would silently miss things. A full reconcile ignores locks.

### Concurrency

The rate limiter is the binding constraint, so parallelism across families only hides latency.
Four API workers by default, one job per family or scope. Blob downloads use a separate pool
(eight) because they hit a third-party host and spend no API budget.

A first full pull is a few hundred requests for a small company, single-digit minutes at 100/min.
20k bank transactions adds roughly 200 requests per account. `fasync status` reports actual spend
so it stops being a guess.

## 8. Time windows

Two distinct windows, sharing one parser. Conflating them would be a bug.

| Flags | Meaning | Mechanism |
| --- | --- | --- |
| `--from`, `--to` | business dates on the records | `ListOptions.FromDate`/`ToDate` where the family supports it; filters the projection otherwise |
| `--changed-since`, `--changed-until` | when the record was last modified | `--changed-since` maps to `updated_since`; `--changed-until` is filtered client-side |

FreeAgent has no `updated_before`, so `--changed-until` cannot be pushed to the server. The help
text says so plainly instead of implying otherwise.

Accepted syntax on all four:

- `2026-03-01`, a bare date. For a business-date window it is a date. For a change window it is
  local midnight.
- RFC3339, for a precise instant on a change window.
- Relative, meaning that long before now, with an optional leading `-`: `30m`, `2h`, `3d`, `2w`,
  `6mo`, `1y`.
- `now`, `today`.

`time.ParseDuration` stops at hours, so `d`, `w`, `mo`, `y` are additions. `m` stays minutes and
months are `mo`. Months and years are calendar-aware via `AddDate`, because "1y ago" on
accounting data has to mean the same date last year, not 365 days.

A window with no upper bound means now. A window with no lower bound means the beginning.

## 9. Blobs and the file trees

Pipeline, resumable at every step:

1. The archive pass writes an `attachments` row per embedded `attachment`, state `pending`.
2. The blob pass takes pending rows, and where `expires_at` has passed, re-resolves the metadata
   through `Attachments.GetURL` for a fresh `content_src`.
3. Downloads with a plain `http.Client` carrying **no OAuth token**, because the host is not
   FreeAgent. Streams to `tmp/`, hashes while writing, renames into `blobs/ab/cd/...`, inserts
   the `blobs` row, sets `attachments.sha256`, state `stored`.
4. Failures increment `attempts` and record `last_error`, retried next run with backoff, never
   blocking the rest of the pull.
5. `fasync blobs verify` re-hashes stored files against the recorded digest.

Generated documents (invoice, estimate, credit note PDFs) take the same path through
`documents`, fetched from the API and therefore inside the rate budget. Opt-in per family and
re-rendered only when the parent's `updated_at` moves, tracked by `rendered_for_updated_at`, so a
routine pull does not re-render every invoice.

`fasync files rebuild` regenerates the JSON records tree. `fasync files relink` regenerates the
symlink views. Both idempotent, both offline, both run at the end of a pull unless `--no-files`.

## 10. Progress and logging

`go-pretty/v6` v6.8.3, `progress` package.

- One `progress.Writer` on stderr, rendered in its own goroutine, `SetAutoStop(false)` so the
  display survives idle gaps between phases.
- One `Tracker` per family, appended as its job starts, plus an overall tracker. Totals become
  determinate after the first page: `LastPage * PerPage` is an upper bound, good enough for a bar.
- A separate tracker for blob downloads in `UnitsBytes`.
- Notes go through the writer's log method so they scroll above the bars instead of tearing
  through them.
- `MarkAsErrored` on a failed family, so a partial run is visible at a glance rather than only in
  the exit code.

**Non-TTY is a first-class mode.** `--progress=auto|always|never`, auto meaning stderr is a
terminal. When off, the same events go to `log/slog` (text or JSON) with one summary line per
family at the end. The UI sits behind one small interface with two implementations, so no engine
code branches on whether a terminal is attached.

## 11. Cron execution

Single-shot, no daemon.

- **Exit codes** distinguish outcomes so monitoring can tell a broken run from one flaky family:
  0 clean, 1 partial (some families failed, state is consistent), 2 configuration or auth error,
  3 lock held by another run, 4 stopped on a budget with work outstanding.
- **A flock on `.lock`** so overlapping ticks cannot corrupt cursors or the blob tmp directory.
  SQLite's WAL handles database concurrency; cursor advancement and `tmp/` need a single writer.
  Fails fast with exit 3 rather than waiting.
- **`--max-duration` and `--max-requests` budgets**, so a run cannot overrun the next tick or eat
  the hourly rate allowance wanted for interactive work. On expiry it commits what it has and
  does not advance cursors past what was actually fetched.
- **`--reconcile-if-due`** checks `last_full_reconcile_at`, so one crontab line covers both
  cadences.
- Sample crontab and systemd timer unit in `docs/scheduling.md`.

## 12. CLI

`fasync`, stdlib `flag` with per-subcommand flag sets, matching `facli`.

```
fasync init
fasync account add|list|remove
fasync auth login|status                       # production needs an explicit flag
fasync probe                                   # updated_since capability per family
fasync pull [--family ...] [--full] [--reconcile-if-due] [--no-blobs] [--no-files]
            [--from ...] [--to ...] [--changed-since ...] [--changed-until ...]
            [--max-duration ...] [--max-requests ...]
fasync reconcile [--family ...]                # full-key sweep, marks deletions
fasync blobs fetch|verify
fasync files rebuild|relink
fasync verify                                  # cross-check against FreeAgent's own reports
fasync reproject [--family ...]                # rebuild SQL projections from layer 1, offline
fasync export --family invoices --format csv|json [--flat] [--from ...] [--to ...]
fasync status                                  # cursors, runs, counts, blob bytes, DB size
fasync sql "select ..."                        # read-only convenience query
```

Global: `--account`, `--data-dir`, `--db`, `--blobs-dir`, `--concurrency`, `--progress`,
`--log-format`.

Everything is `internal/`; nothing is exported. The write verbs do not exist and will be their
own verbs when they do, never a flag on `pull`.

## 13. Safety

- The pull path builds its client with `WithReadOnly()` and there is **no code path that can
  build a writable one**. Structural, not a flag: the constructor for pull mode takes no
  writable parameter. The write path gets its own constructor and its own verbs.
- Production is opt-in, per the SDK's existing convention.
- Anonymised fixtures only. Real company data never enters the repo, a test, or a commit.
- Secrets never land in the database. Tokens stay in the SDK's `0600` store.
- No third-party host ever receives the OAuth token; the blob downloader uses a bare client.

## 14. Verification

The mirror should prove itself, not just report success.

- **Cross-check against FreeAgent's own arithmetic.** Pull the trial balance and compare it to
  the sum of locally mirrored transactions per nominal code. A trial balance sums to zero and the
  SDK already asserts that; if the local sums match FreeAgent's, the transaction mirror is
  complete and exact end to end. Same idea for profit and loss over a period. The strongest
  correctness signal available, and nearly free.
- Per-family counts from a completed reconcile, compared against the previous one.
- Blob digests re-verified on demand.
- Every run writes a `sync_runs` row, so "when did this last actually work" is a query.

## 15. Testing

Two accounts, and the split between them is the whole testing strategy.

- **The accountant-managed production company** is the real target. It is read-only, always, and
  no test points at it. Its only role in development is `facli schema`, which reports field paths
  and type classifications and never a value, when a shape needs confirming.
- **The sandbox company** from the SDK work is where the live suite runs. Build-tagged
  `integration`, never in PR CI, same discipline as the SDK: the read half refuses production
  unless `FREEAGENT_ALLOW_PRODUCTION=1`, and there is no write half at all until phase 6.

Unit tests, no network:

- The store against a real temporary SQLite file rather than a mock. That is the payoff of an
  embedded database: the tests exercise the actual SQL, the actual constraints, and the actual
  migrations.
- The engine against `httptest` serving canned envelopes with `Link` headers, covering the
  page-walk, the cursor arithmetic, and an interrupted run resuming.
- The time-window parser gets a table test, including the `m` versus `mo` split and the
  calendar-aware year boundary across a leap day.
- A seeded fake API for the reconcile path: sync N records, remove one server-side, reconcile,
  assert the soft delete lands and nothing else moved.
- Money round-trips: every decimal survives TEXT and `_e6` and back, and a seven-decimal value
  fails the write rather than rounding.

Fixtures are anonymised, from the sandbox, never from production. Same rule as the SDK and the
same reason.

## 16. Phasing

| Phase | Deliverable |
| --- | --- |
| 0 | Repo, CI mirroring the SDK's, store with migrations, accounts, auth, the time-window parser, the UI interface with both implementations. `init`, `account`, `auth`, `status` |
| 1 | Generic archive: raw pager, cursors, `pull`, `reconcile`, `probe`, locking, exit codes, budgets. Every enumerable family mirrored, plus `account_locks` and `currencies` |
| 2 | What was actually asked for: bank transactions and explanations via the account-scoped strategy; the blob store with attachments from bills, expenses and explanations; the records tree and the symlink views |
| 3 | SQL projections for the families worth querying, `reproject`, `export`, `sql` |
| 4 | Reports as dated snapshots, period-addressed filings, generated PDFs, `verify` |
| 5 | Scheduling docs, hardening, the Files-area gap documented where a user will see it |
| 6 | Write path, separate design pass |

Phase 1 carries the leverage: raw archiving is generic, so it covers every family at once rather
than one at a time.

## 17. The write path, reserved

Not being built. What this design keeps possible:

- `identity_map` exists from the start. Every cross-reference in a FreeAgent payload is a URL on
  the source host, so replicating into a second company means translating every reference, and
  that mapping needs somewhere to live.
- `records.local_body`, `local_dirty_at`, `push_state`, `pushed_at`, `push_error`, `source_url`,
  `accounts.role`, `accounts.writable`, and `sync_runs.direction` are created and unused.
- The parents-first family ordering is the apply order.
- Unbounded `record_versions` gives enough history for either last-writer-wins or
  source-of-truth-wins, without having to choose now.
- The shape will be `plan` then `apply`, terraform-style: a diff read before anything moves.

Questions deferred with it: conflict policy, whether the second company is a mirror or a peer,
and what happens to records the API cannot recreate at all. Bank transactions arrive by feed or
statement upload, so a true two-way sync cannot recreate them as transactions.

## 18. Risks

| Risk | Handling |
| --- | --- |
| Files-area uploads are invisible to the API | no mitigation exists; surfaced in `status` and the README so the archive never implies completeness it lacks |
| Page-number pagination skips a record edited mid-walk | overlap window plus reconcile; documented |
| A family silently ignores `updated_since` | `probe`, one request, result stored |
| Attachment URLs expire mid-run | re-resolve metadata before download |
| Deletions are invisible without a sweep | reconcile is the only mechanism; its due-ness shows in `status` |
| An ad-hoc window corrupts the incremental cursor | explicit windows never write cursors |
| First full pull hits the rate limit | limiter already sits under the caps; spend reported per run |
| Overlapping cron runs | flock, fail fast with a distinct exit code |
| Unbounded `record_versions` growth | accepted deliberately; history is a goal, and `status` reports size |
| Accounting data at rest on a laptop | `0700`, `0600`, full-disk encryption, documented |
| Access to the accountant-managed company | needs a FreeAgent user with access to it; a prerequisite, not a code problem |

## 19. Decisions taken

Recorded so the reasoning is not re-litigated later.

| Decision | Choice | Why |
| --- | --- | --- |
| Storage engine | SQLite via `modernc.org/sqlite`, pure Go | no cgo, no server, standard file any tool can open |
| At-rest encryption | none | an encrypted database readable by one library defeats portability; full-disk encryption instead |
| Source of truth | raw JSON archive, everything else derived | the API returns fields the docs omit; a typed-only mirror loses them permanently |
| Money in SQL | exact `TEXT` plus scaled `INTEGER` at scale 6 | SQLite has no decimal type and `REAL` breaks the SDK's hardest rule |
| Blob storage | content-addressed files, not BLOB columns | dedupe, integrity checking, and a database small enough to copy |
| Accounts | rows in the database | no config file format to choose, no third surface to keep consistent |
| Companies | production plus the existing sandbox | production stays read-only forever; the live suite runs against the sandbox |
| Records tree | one pretty JSON file per record, `<id>.versions/` beside it | stable paths, ordinary diff tools work, greppable |
| Browsable tree | symlinks, three views, rebuilt idempotently | free to regenerate, no duplicated bytes |
| History | unbounded, never pruned | knowing what the accountant changed and when is a goal, not a side effect |
| Export | faithful by default, `--flat` for spreadsheets | the faithful shape round-trips; flattening is lossy and belongs behind a flag |
| Scheduling | cron or a timer, single shot | no daemon to supervise; budgets and a lock keep runs from colliding |
| Package surface | everything `internal/`, nothing exported | no second consumer exists; decide when one does |
| Write path | deferred, columns reserved | cheaper than migrating an archive later |

### Still unknown, to settle during the build

- Whether every family actually honours `updated_since`. `fasync probe` answers this against the
  live account rather than by guessing, and phase 1 is where that gets run.
- How much sits unreachable in the Files area. Worth checking in the web interface early, because
  it bounds what "complete archive" can honestly mean.
