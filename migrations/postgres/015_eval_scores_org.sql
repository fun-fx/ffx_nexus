-- 015: attribute eval scores to an organization.
--
-- eval_scores recorded trace_id and user_id but never org_id, so every read of
-- evaluation quality was installation-wide. A single-customer deployment still
-- divides teams and departments into orgs, and a score row carries the metric,
-- the rationale text and the judged model — so one team's admin could read the
-- quality and safety findings for another team's traffic, including the
-- rationale strings a judge model wrote about their prompts.
--
-- Additive, per the expand/contract rule in migrations/README.md:
--
--   * The column is NOT NULL with a DEFAULT. The default — not nullability — is
--     what keeps version N of the binary working against this schema: N's
--     INSERT names its columns and omits org_id, so Postgres supplies 'default'
--     and the write succeeds. Rolling the application back therefore needs no
--     schema change. (NOT NULL is the stronger choice for the same
--     compatibility: it makes "a new row with no org" unrepresentable rather
--     than merely discouraged, so there is no NULL case for reads to handle.)
--   * No row is deleted and no column is dropped.
--
-- BACKFILL RULE. Attribution of pre-migration rows depends on how many orgs the
-- installation actually uses, because "put it in the default org" is a correct
-- statement about a single-org deployment and a guess about any other.
--
-- "Uses" is measured as the number of distinct orgs that own a user, NOT the
-- number of rows in `organizations`. 001_init.sql unconditionally seeds an org
-- called 'default', so the table never holds fewer than one row and a customer
-- who created their own org and moved everyone into it would look multi-org by
-- that measure — and their own history would be withheld from them for nothing.
--
--   * One org in use: every historical row belongs to it. Not an inference —
--     there was nowhere else for the traffic to have come from.
--   * More than one: rows are attributed from the score's own user where that
--     user still exists, which is a real signal rather than a guess. Rows with
--     no usable user (org-level or legacy traffic) are parked in
--     'unattributed', a scope no org's reads match, so they surface to nobody
--     until an operator decides where they belong. Reclaiming them is a
--     deliberate step documented in docs/customer-self-hosted-integrations.md;
--     it is not something a migration should decide on an operator's behalf.
--   * No users at all: an API-key-only installation, where the gateway stamps
--     'default' on live traffic anyway. Historical rows keep that same value,
--     so old and new data agree.

ALTER TABLE eval_scores
    ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'default';

DO $$
DECLARE
    orgs_in_use integer;
    sole_org    text;
BEGIN
    SELECT count(DISTINCT org_id) INTO orgs_in_use FROM users;

    IF orgs_in_use = 1 THEN
        SELECT DISTINCT org_id INTO sole_org FROM users;
        -- Skipped when the sole org is already literally 'default', where the
        -- ADD COLUMN default has put them.
        IF sole_org IS DISTINCT FROM 'default' THEN
            UPDATE eval_scores SET org_id = sole_org WHERE org_id = 'default';
        END IF;

    ELSIF orgs_in_use > 1 THEN
        -- Attribute from the owning user. Bounded to rows still sitting at the
        -- ADD COLUMN default so a re-run cannot overwrite an attribution a
        -- later insert set correctly.
        UPDATE eval_scores es
        SET org_id = u.org_id
        FROM users u
        WHERE es.user_id = u.id
          AND es.user_id <> ''
          AND es.org_id = 'default'
          AND u.org_id <> 'default';

        -- Whatever is left at 'default' and has no matching user cannot be
        -- attributed. Park it out of every org's reach. Rows whose user IS a
        -- member of an org genuinely named 'default' are excluded by the
        -- EXISTS check and correctly stay put.
        UPDATE eval_scores es
        SET org_id = 'unattributed'
        WHERE es.org_id = 'default'
          AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = es.user_id);
    END IF;
END
$$;

-- Reads are always "this org, this metric, over this window" and "this org,
-- this trace", so org_id leads both indexes. The pre-existing indexes stay:
-- dropping them would slow the scheduler's cross-org sweeps, which legitimately
-- query without an org filter.
CREATE INDEX IF NOT EXISTS idx_eval_scores_org_metric_ts
    ON eval_scores (org_id, metric, timestamp DESC);

CREATE INDEX IF NOT EXISTS idx_eval_scores_org_trace
    ON eval_scores (org_id, trace_id);
