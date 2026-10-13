-- Alerting state and history. Idempotent. {{schema}} is replaced by the schema
-- name plus a dot (or nothing): see pgstore.SchemaSQL.
--
-- alert_instances is the CURRENT state of every label set of every rule that is
-- pending, firing, or resolved-but-not-yet-delivered. alert_events is the
-- append-only history of transitions.

CREATE TABLE IF NOT EXISTS {{schema}}alert_instances (
    tenant       TEXT        NOT NULL,
    env          TEXT        NOT NULL,
    rule         TEXT        NOT NULL,
    fingerprint  TEXT        NOT NULL,
    labels       JSONB       NOT NULL,
    annotations  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    state        TEXT        NOT NULL,
    active_at    TIMESTAMPTZ NOT NULL,
    firing_at    TIMESTAMPTZ,
    cleared_at   TIMESTAMPTZ,
    resolved_at  TIMESTAMPTZ,
    resolve_sent BOOLEAN     NOT NULL DEFAULT FALSE,
    last_eval_at TIMESTAMPTZ NOT NULL,
    last_value   DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (tenant, env, rule, fingerprint),
    CONSTRAINT ck_alert_instances_state CHECK (state IN ('pending', 'firing', 'resolved'))
);

CREATE INDEX IF NOT EXISTS idx_alert_instances_tenant_state ON {{schema}}alert_instances (tenant, state);

CREATE TABLE IF NOT EXISTS {{schema}}alert_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant      TEXT        NOT NULL,
    env         TEXT        NOT NULL,
    rule        TEXT        NOT NULL,
    fingerprint TEXT        NOT NULL,
    labels      JSONB       NOT NULL,
    transition  TEXT        NOT NULL,
    value       DOUBLE PRECISION NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    CONSTRAINT ck_alert_events_transition CHECK (transition IN ('pending', 'firing', 'resolved', 'inactive', 'removed'))
);

CREATE INDEX IF NOT EXISTS idx_alert_events_tenant_at ON {{schema}}alert_events (tenant, at DESC);
CREATE INDEX IF NOT EXISTS idx_alert_events_rule_at ON {{schema}}alert_events (tenant, env, rule, at DESC);
