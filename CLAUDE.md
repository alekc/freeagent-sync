# Working on freeagent-sync

Context for anyone, human or agent, picking this repository up. Read this
before changing anything; it records decisions whose reasons are not visible
in the diff. The long form is [docs/design.md](docs/design.md).

## What this is

`fasync`, a CLI that mirrors a FreeAgent company into a local SQLite archive
with its attachments. It consumes `github.com/alekc/freeagent`, which is the
library and deliberately has no sync engine of its own.

The company being mirrored is administered by somebody else. That single fact
drives most of what follows.

Go 1.26 is the floor, matching the SDK.

## It must not be able to write

The read path builds its client with the SDK's `WithReadOnly()`, which refuses
every mutating verb inside `Client.newRequest`.

**The guarantee is structural, not procedural.** The constructor used by the
pull path takes no "writable" parameter, so there is no argument anyone can
pass to get a writable client. When the write path lands it gets its own
constructor and its own verbs (`plan`, `apply`), never a flag on `pull`.

Do not add one. Do not add a bypass "just for" some endpoint. Note that
`account_locks` is a PUT and DELETE endpoint, so this is load-bearing on a
family the tool already reads.

## Non-negotiables

**SQLite is the only source of truth.** The JSON record tree, the symlink
tree, and every typed table are derived and must be rebuildable offline. Never
write them in parallel with the archive: a crash would leave the two
disagreeing with no way to tell which was right.

**Bodies are archived verbatim.** `records.body` holds the response exactly as
received. The SDK found a dozen doc-versus-reality discrepancies and eight
undocumented fields, so anything less loses data that can only be recovered by
going back to a live production account.

**Money is never a float.** `TEXT` for the exact decimal, plus a scaled
`INTEGER` at scale 6 (`*_e6`) so SQL can sum. A value with more precision than
the scale fails the write; it is never rounded silently.

**Records are never deleted.** `deleted_at` is a soft delete. A record the
accountant removed is exactly the thing this archive exists to keep.

**Timestamps use the fixed-width UTC layout in `store.go`.** RFC3339Nano trims
trailing zeros, which would break lexicographic ordering of stored text.

**Never commit anything from a real company.** Fixtures are anonymised.
Production is read-only, always, and no test points at it.

## Layout

```
cmd/fasync/           the CLI, stdlib flag with per-subcommand flag sets
  session.go          lock, archive, client and engine, assembled once
internal/store/       schema, migrations, accounts, records, cursors, runs
  migrations/*.sql    stepped by SQLite's own user_version
internal/api/         read-only client and the generic pager over any family
internal/engine/      classification, pull, reconcile, probe, blobs, budgets
internal/blob/        content-addressed file store for attachments
internal/tree/        the derived JSON record tree and the symlink views
internal/export/      CSV and JSON output, faithful and flat
internal/auth/        the OAuth dance, keyed per account slug
internal/lock/        one run at a time per data directory
internal/timeframe/   the bound parser shared by every time flag
internal/ui/          progress bars and the log fallback behind one interface
internal/config/      paths and credentials, resolved once at startup
docs/design.md        the plan, including what the API cannot do
```

## Things that will bite

**The DSN needs its `file:` prefix.** modernc's driver strips the query string
without it, so every pragma silently does nothing and the archive runs with no
foreign keys. `verifyPragmas` reads them back at open for exactly this reason.

**`flag` stops at the first positional.** `parseArgs` permutes so flags work
after the verb, the same helper facli uses. Use it, not `fs.Parse`.

**`time.AddDate` normalises an overflowing day**, so a month before 31 March
is 3 March. `timeframe.addMonths` clamps to the end of the target month.
Do not replace it with AddDate.

**FreeAgent's Files area is unreachable.** Attaching a file removes it from
that area, and there is no files endpoint, so unattached uploads cannot be
mirrored by anything. `fasync status` says so on every run. Do not quietly
drop that notice.

**An explicit time window makes a run ad-hoc**, and an ad-hoc run must never
advance the stored cursor. Otherwise a narrow manual pull leaves the next
scheduled run believing it is caught up. `store.AdvancesCursor` is the single
place that decides, and `TestAdHocRunNeverAdvancesTheCursor` guards it.

**A partial walk advances nothing.** Pagination is by page number, so a run
stopped by an error or a budget has no guarantee about which records it saw.
It must not move the cursor and must not sweep. Re-reading next time is the
cheap option; a wrong sweep deletes live records.

**The cursor comes from the payloads, never from the clock.** `time.Now()`
would skip anything written while the run was in flight. This is subtle enough
that a mutation test was used to confirm the suite catches it.

**A family that fails does not fail the run.** One flaky endpoint should not
cost a night's sync of everything else, so failures are collected and the run
reports partial with a distinct exit code.

**A bank-scoped family is swept only when every account has been read.** The
read fans out per bank account but `deleted_at` has no scope, so sweeping
after a partial fan-out deletes the accounts that were not reached. The sweep
is also bounded by the *earliest* job start, not the latest: bounded by the
latest, everything the first account re-saw would be deleted. Both are
mutation-tested, and the second needs `Concurrency: 1` to reproduce.

**Attachment downloads must carry no OAuth token.** content_src points at a
third-party host. The downloader is a bare `http.Client` on purpose; do not
give it the API client's transport.

**Five families live at a path that is not their name.** transactions is at
`accounting/transactions`, the reports are under `accounting/`, and
income_tax_returns is served as `self_assessment_returns`. Anything keyed on a
request path has to resolve it back through the registry; the test stub does.

**A singleton is archived as its whole envelope.** company, email_addresses and
cis_bands each wrap their content differently and one uses a key matching
neither its singular nor its plural name, so unwrapping would be three special
cases that could each lose data. The URL is the endpoint, since there is
nothing else to address, and such a record is never swept.

**Reports are snapshotted, not upserted.** Last quarter's profit and loss is a
different answer to a different question. An unchanged report does not append a
snapshot, or an hourly schedule fills the table.

**`Verify` runs on an engine with no API client.** Use `NewOffline`. The base
host for the reference check comes from the archived URLs, not from a client,
so nothing there may reach for `e.client`. A nil dereference here survived the
whole test suite once because every other test has a client.

**A check that cannot run is skipped, never passed.** "Nothing was checked" and
"nothing is wrong" are different answers, and conflating them is the most
misleading thing this tool could do. The same applies to coverage: attachments
are reported as read "via parents", not as "not yet", because they are archived.

**Payroll is addressed by the year a tax year ends in.** April 2025 to March
2026 is year 2026, and there is no endpoint listing which years exist. The
range comes from the company record when it is archived, and a year that 404s
is the normal answer across most of it, not a failure.

**Payslips only arrive on the per-period fetch.** The year response lists
periods without them, so archiving the year alone mirrors the shape of payroll
with none of its content.

**PDF renders cost an API request each.** Keyed on the parent's
remote_updated_at, so a record is re-rendered exactly when it moves. Not part
of a routine pull: a thousand invoices is a thousand requests, and that has to
be a deliberate act.

**Four families cannot be read at their own path.** notes needs a contact or
project parameter (400 without it), income_tax_returns exists only at
`users/:id/self_assessment_returns` (the registry Path is a suffix), the bank
families need a bank_account filter, and payroll needs a year. All four were
live 400s and 404s against a real company before they were classified.

**A 403 or 404 on a scoped family is a fact about the company.** A plan or a
role can exclude a feature entirely, and reporting that as a failure makes
every run of an ordinary company report partial forever. Those are marked
Unavailable, listed separately from failures, and never swept.

**updated_since is only a question for a paged collection.** Probing a
singleton, report or year-addressed family produced errors that said nothing
about the API. `Probeable` decides, and the probe makes no request for the
families it excludes.

**Every job checks the budget before it starts.** Without the check in runJob,
each job gets one request through regardless, so `--max-requests 3` was
overrun by the number of jobs in the plan rather than by the few in flight.

**Never aggregate money out of `body` in SQL.** json_extract returns the exact
decimal as TEXT and SQLite coerces TEXT to REAL to add it, so
`sum(json_extract(body,'$.total_value'))` reintroduces the float error this
project refuses everywhere else. `record_numbers.value_e6` is the exact
integer; sum that. A value beyond the scale gets NULL, never a rounded number.

**The faithful export must round-trip.** CSV cells have to be text, so a
nested value is rendered as its JSON there; the JSON export must emit the
original values instead. Stringifying an array in JSON output makes it parse
but stops it being the payload, which defeats the only guarantee the faithful
shape offers.

**The file views are hardlinks, not symlinks.** A symlink is resolved before
its name is examined, and the blobs are named by content hash with no
extension, so a viewer following one lands on a file that looks like nothing:
on macOS the PDFs opened in a text editor showing raw PDF source. A hardlink
has no target to resolve, so the entry's own name is the file's name. Do not
change the default back. `auto` falls back to symlink only when the filesystem
refuses to hardlink.

**Export data goes to stdout, everything else to stderr.** A redirected export
must contain the export and nothing else.

## Fail early, in code

Constraints belong in guards, not in prose. Existing examples: pragma
read-back at open, schema version refusal, slug validation before the insert,
credential pairing checked at startup, STRICT tables, and an inverted time
window rejected locally rather than sent to the API to come back empty.

Add to that list rather than adding a warning to a doc. More of them:
`SelectFamilies` refuses an unknown or unsupported family by name instead of
omitting it, the pager errors on a missing plural key rather than yielding
nothing, and the global flags are validated at parse time so a typo is
reported as a typo.

## Testing

```bash
make lint test            # unit, no network
make test-integration     # live, sandbox only
```

The store is tested against a real temporary SQLite file, not a mock. That is
the point of an embedded database: the tests exercise the actual SQL, the
actual constraints, and the actual migrations.

Assert shape and invariants rather than the values of one capture.

## Conventions

Commit messages explain **why**, not what. Sign off with `git commit -s`. No
AI attribution trailers of any kind.

Comments cap at 80 characters per line and four lines per block, and say what
the code cannot: the consequence of getting it wrong, or the upstream quirk
being worked around.

No em dashes in anything the repository publishes.
