-- Supports DeploymentRepository.ListByStatus, which both the reconciler and
-- the abandoned-deployment sweeper call at every startup. Without it those
-- are sequential scans over the full deployment history.
--
-- Not CONCURRENTLY: golang-migrate runs each migration inside a transaction,
-- and CREATE INDEX CONCURRENTLY cannot run in one. The table is small enough
-- that the brief lock is harmless; revisit by building it by hand if this
-- ever grows past a few hundred thousand rows.
CREATE INDEX IF NOT EXISTS idx_deployments_status_created
    ON deployments (status, created_at DESC);
