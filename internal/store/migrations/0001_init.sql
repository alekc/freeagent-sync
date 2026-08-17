-- Initial archive schema. Layer 1 (records, record_versions) is the source of
-- truth; every other table is either control state or derived and rebuildable.
-- Tables are STRICT so a wrong type fails at write rather than at read.
-- Timestamps are TEXT in the fixed-width UTC layout defined in store.go.

CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE accounts (
    id          INTEGER PRIMARY KEY,
    slug        TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL DEFAULT '',
    environment TEXT    NOT NULL,
    company_url TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL,
    disabled_at TEXT,

    -- RESERVED for the write path, see docs/design.md section 17. Created so
    -- adding them later is not a migration against an irreplaceable archive.
    role     TEXT    NOT NULL DEFAULT 'mirror',
    writable INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Declared before records because records references it.
CREATE TABLE sync_runs (
    id               INTEGER PRIMARY KEY,
    account_id       INTEGER NOT NULL REFERENCES accounts(id),
    mode             TEXT    NOT NULL,
    started_at       TEXT    NOT NULL,
    finished_at      TEXT,
    families         TEXT    NOT NULL DEFAULT '',
    window_from      TEXT,
    window_to        TEXT,
    changed_since    TEXT,
    changed_until    TEXT,
    requests         INTEGER NOT NULL DEFAULT 0,
    records_upserted INTEGER NOT NULL DEFAULT 0,
    records_deleted  INTEGER NOT NULL DEFAULT 0,
    bytes_downloaded INTEGER NOT NULL DEFAULT 0,
    outcome          TEXT,
    error            TEXT,

    -- RESERVED for the write path.
    direction TEXT NOT NULL DEFAULT 'pull'
) STRICT;

CREATE INDEX sync_runs_recent ON sync_runs(account_id, started_at DESC);

-- Layer 1. body holds the response verbatim, so a field this tool does not
-- model yet is still archived and recoverable by reprojection.
CREATE TABLE records (
    account_id        INTEGER NOT NULL REFERENCES accounts(id),
    family            TEXT    NOT NULL,
    url               TEXT    NOT NULL,
    remote_id         TEXT    NOT NULL DEFAULT '',
    body              TEXT    NOT NULL,
    body_sha256       TEXT    NOT NULL,
    remote_updated_at TEXT,
    first_seen_at     TEXT    NOT NULL,
    last_seen_at      TEXT    NOT NULL,

    -- Soft delete. Rows are never removed: a record the accountant deleted is
    -- exactly the thing this archive exists to keep.
    deleted_at     TEXT,
    deleted_by_run INTEGER REFERENCES sync_runs(id),

    -- Set from account_locks when the record falls in a closed period.
    -- Advisory: a lock stops transaction edits, not every metadata change.
    final_at TEXT,

    -- RESERVED for the write path.
    local_body     TEXT,
    local_dirty_at TEXT,
    push_state     TEXT,
    pushed_at      TEXT,
    push_error     TEXT,
    source_url     TEXT,

    PRIMARY KEY (account_id, url)
) STRICT;

CREATE INDEX records_family_updated
    ON records(account_id, family, remote_updated_at);
CREATE INDEX records_family_seen
    ON records(account_id, family, last_seen_at);
CREATE INDEX records_live
    ON records(account_id, family) WHERE deleted_at IS NULL;

-- Append only, never pruned. Keeping every version is how "what did the
-- accountant change, and when" stays answerable.
CREATE TABLE record_versions (
    id                INTEGER PRIMARY KEY,
    account_id        INTEGER NOT NULL,
    url               TEXT    NOT NULL,
    body              TEXT    NOT NULL,
    body_sha256       TEXT    NOT NULL,
    remote_updated_at TEXT,
    seen_at           TEXT    NOT NULL,

    FOREIGN KEY (account_id, url) REFERENCES records(account_id, url)
) STRICT;

CREATE INDEX record_versions_history
    ON record_versions(account_id, url, seen_at);

CREATE TABLE blobs (
    sha256       TEXT PRIMARY KEY,
    size         INTEGER NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    stored_at    TEXT    NOT NULL,
    verified_at  TEXT
) STRICT;

-- One row per attachment seen on a bill, expense or bank transaction
-- explanation. content_src is time limited, so expires_at decides whether the
-- metadata needs re-resolving before the bytes can be fetched.
CREATE TABLE attachments (
    account_id    INTEGER NOT NULL REFERENCES accounts(id),
    url           TEXT    NOT NULL,
    parent_url    TEXT    NOT NULL,
    family        TEXT    NOT NULL,
    file_name     TEXT    NOT NULL DEFAULT '',
    content_type  TEXT    NOT NULL DEFAULT '',
    file_size     INTEGER NOT NULL DEFAULT 0,
    content_src   TEXT    NOT NULL DEFAULT '',
    expires_at    TEXT,
    sha256        TEXT REFERENCES blobs(sha256),
    state         TEXT    NOT NULL DEFAULT 'pending',
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    first_seen_at TEXT    NOT NULL,

    PRIMARY KEY (account_id, url)
) STRICT;

CREATE INDEX attachments_outstanding
    ON attachments(account_id, state) WHERE state <> 'stored';
CREATE INDEX attachments_parent ON attachments(account_id, parent_url);

-- Generated documents: invoice, estimate and credit note PDFs. Re-rendered
-- only when the parent moves, which rendered_for_updated_at tracks.
CREATE TABLE documents (
    account_id              INTEGER NOT NULL REFERENCES accounts(id),
    parent_url              TEXT    NOT NULL,
    kind                    TEXT    NOT NULL,
    sha256                  TEXT REFERENCES blobs(sha256),
    rendered_at             TEXT,
    rendered_for_updated_at TEXT,

    PRIMARY KEY (account_id, parent_url, kind)
) STRICT;

-- One row per family and scope. scope is empty for plain collections and
-- carries the bank account URL for the families that require one.
CREATE TABLE sync_state (
    account_id             INTEGER NOT NULL REFERENCES accounts(id),
    family                 TEXT    NOT NULL,
    scope                  TEXT    NOT NULL DEFAULT '',
    cursor_updated_at      TEXT,
    last_full_reconcile_at TEXT,
    supports_updated_since INTEGER,
    last_run_id            INTEGER REFERENCES sync_runs(id),

    PRIMARY KEY (account_id, family, scope)
) STRICT;

-- What `fasync probe` established about a family, so behaviour is recorded
-- rather than guessed on every run.
CREATE TABLE capabilities (
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    family     TEXT    NOT NULL,
    probe      TEXT    NOT NULL,
    result     TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '',
    probed_at  TEXT    NOT NULL,

    PRIMARY KEY (account_id, family, probe)
) STRICT;

-- Which periods the accountant has closed. Observed over time rather than
-- overwritten, because when a period closed is itself worth knowing.
CREATE TABLE account_locks (
    account_id         INTEGER NOT NULL REFERENCES accounts(id),
    observed_at        TEXT    NOT NULL,
    locked_to_date     TEXT,
    user_lock          INTEGER,
    earliest_lock_date TEXT,
    latest_lock_date   TEXT,

    PRIMARY KEY (account_id, observed_at)
) STRICT;

-- Reports are derived and point in time, so they are snapshotted rather than
-- upserted: last quarter's profit and loss is not a stale row.
CREATE TABLE report_snapshots (
    id          INTEGER PRIMARY KEY,
    account_id  INTEGER NOT NULL REFERENCES accounts(id),
    report      TEXT    NOT NULL,
    from_date   TEXT,
    to_date     TEXT,
    taken_at    TEXT    NOT NULL,
    body        TEXT    NOT NULL,
    body_sha256 TEXT    NOT NULL
) STRICT;

CREATE INDEX report_snapshots_lookup
    ON report_snapshots(account_id, report, taken_at DESC);

-- RESERVED for the write path. Every cross-reference in a payload is a URL on
-- the source host, so replicating into another company needs a translation.
CREATE TABLE identity_map (
    source_account_id INTEGER NOT NULL REFERENCES accounts(id),
    source_url        TEXT    NOT NULL,
    target_account_id INTEGER NOT NULL REFERENCES accounts(id),
    target_url        TEXT    NOT NULL,
    linked_at         TEXT    NOT NULL,

    PRIMARY KEY (source_account_id, source_url, target_account_id)
) STRICT;
