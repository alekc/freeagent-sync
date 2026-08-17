-- Exact numeric projection, and views that make the archive readable.
--
-- The archive keeps bodies verbatim, which is right for fidelity but leaves a
-- trap in SQL: json_extract returns an exact decimal as TEXT, and SQLite
-- coerces TEXT to REAL to aggregate it. sum() over '0.10' and '0.20' gives
-- 0.30000000000000004, which is exactly the float error this project refuses
-- to accept anywhere else.
--
-- record_numbers carries every numeric field twice: the exact text as it
-- arrived, and an integer scaled by a million. Summing value_e6 is exact.
CREATE TABLE record_numbers (
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    url        TEXT    NOT NULL,
    family     TEXT    NOT NULL,
    field      TEXT    NOT NULL,

    -- The value exactly as FreeAgent sent it. Always populated.
    text_value TEXT    NOT NULL,
    -- text_value scaled by 1e6. NULL when the value needs more than six
    -- decimal places, or is too large for an integer, rather than rounding
    -- silently to make the column look complete.
    value_e6   INTEGER,

    PRIMARY KEY (account_id, url, field),
    FOREIGN KEY (account_id, url) REFERENCES records(account_id, url)
) STRICT;

CREATE INDEX record_numbers_field ON record_numbers(account_id, family, field);

-- Convenience views. A field the API stops sending reads as NULL rather than
-- breaking the view, so these degrade instead of failing.
CREATE VIEW v_invoices AS
SELECT r.url,
       r.remote_id                                 AS id,
       json_extract(r.body, '$.reference')         AS reference,
       json_extract(r.body, '$.dated_on')          AS dated_on,
       json_extract(r.body, '$.due_on')            AS due_on,
       json_extract(r.body, '$.status')            AS status,
       json_extract(r.body, '$.currency')          AS currency,
       json_extract(r.body, '$.total_value')       AS total_value,
       json_extract(r.body, '$.due_value')         AS due_value,
       json_extract(r.body, '$.contact')           AS contact_url,
       json_extract(r.body, '$.project')           AS project_url,
       r.remote_updated_at                         AS updated_at,
       r.deleted_at
FROM records r
WHERE r.family = 'invoices';

CREATE VIEW v_bills AS
SELECT r.url,
       r.remote_id                           AS id,
       json_extract(r.body, '$.reference')   AS reference,
       json_extract(r.body, '$.dated_on')    AS dated_on,
       json_extract(r.body, '$.due_on')      AS due_on,
       json_extract(r.body, '$.status')      AS status,
       json_extract(r.body, '$.total_value') AS total_value,
       json_extract(r.body, '$.contact')     AS contact_url,
       json_extract(r.body, '$.category')    AS category_url,
       r.remote_updated_at                   AS updated_at,
       r.deleted_at
FROM records r
WHERE r.family = 'bills';

CREATE VIEW v_expenses AS
SELECT r.url,
       r.remote_id                              AS id,
       json_extract(r.body, '$.dated_on')       AS dated_on,
       json_extract(r.body, '$.description')    AS description,
       json_extract(r.body, '$.gross_value')    AS gross_value,
       json_extract(r.body, '$.sales_tax_value') AS sales_tax_value,
       json_extract(r.body, '$.category')       AS category_url,
       json_extract(r.body, '$.user')           AS user_url,
       r.remote_updated_at                      AS updated_at,
       r.deleted_at
FROM records r
WHERE r.family = 'expenses';

CREATE VIEW v_bank_transactions AS
SELECT r.url,
       r.remote_id                                     AS id,
       json_extract(r.body, '$.dated_on')              AS dated_on,
       json_extract(r.body, '$.amount')                AS amount,
       json_extract(r.body, '$.description')           AS description,
       json_extract(r.body, '$.full_description')      AS full_description,
       json_extract(r.body, '$.unexplained_amount')    AS unexplained_amount,
       json_extract(r.body, '$.bank_account')          AS bank_account_url,
       r.remote_updated_at                             AS updated_at,
       r.deleted_at
FROM records r
WHERE r.family = 'bank_transactions';

CREATE VIEW v_transactions AS
SELECT r.url,
       r.remote_id                                AS id,
       json_extract(r.body, '$.dated_on')         AS dated_on,
       json_extract(r.body, '$.description')      AS description,
       json_extract(r.body, '$.nominal_code')     AS nominal_code,
       json_extract(r.body, '$.category_name')    AS category_name,
       json_extract(r.body, '$.debit_value')      AS debit_value,
       json_extract(r.body, '$.source_item_url')  AS source_item_url,
       r.remote_updated_at                        AS updated_at,
       r.deleted_at
FROM records r
WHERE r.family = 'transactions';

CREATE VIEW v_contacts AS
SELECT r.url,
       r.remote_id                                    AS id,
       json_extract(r.body, '$.organisation_name')    AS organisation_name,
       json_extract(r.body, '$.first_name')           AS first_name,
       json_extract(r.body, '$.last_name')            AS last_name,
       json_extract(r.body, '$.email')                AS email,
       json_extract(r.body, '$.status')               AS status,
       r.remote_updated_at                            AS updated_at,
       r.deleted_at
FROM records r
WHERE r.family = 'contacts';

-- Attachments and rendered documents with the blob that holds them, which is
-- what turns "where is that receipt" into one query.
CREATE VIEW v_files AS
SELECT a.account_id,
       a.family,
       a.parent_url,
       a.file_name,
       a.content_type,
       b.size,
       a.sha256,
       'attachment' AS origin
FROM attachments a
JOIN blobs b ON b.sha256 = a.sha256
WHERE a.sha256 IS NOT NULL
UNION ALL
SELECT d.account_id,
       coalesce(r.family, '') AS family,
       d.parent_url,
       r.remote_id || '.' || d.kind AS file_name,
       'application/' || d.kind     AS content_type,
       b.size,
       d.sha256,
       'rendered' AS origin
FROM documents d
JOIN blobs b ON b.sha256 = d.sha256
LEFT JOIN records r
  ON r.account_id = d.account_id AND r.url = d.parent_url
WHERE d.sha256 IS NOT NULL;
