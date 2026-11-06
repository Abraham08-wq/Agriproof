-- AgriProof schema v1: cooperatives, farmers, deliveries and the sealed ledger

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS cooperatives (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 2 AND 120),
    county      TEXT NOT NULL DEFAULT '',
    currency    TEXT NOT NULL DEFAULT 'KES' CHECK (length(currency) = 3),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Crops a cooperative buys, with the most a farm can plausibly grow per acre in a season
-- (used by the AI to spot produce brought in from elsewhere).
CREATE TABLE IF NOT EXISTS crops (
    code               TEXT PRIMARY KEY,              -- maize, coffee, tea, beans, ...
    name               TEXT NOT NULL,
    unit               TEXT NOT NULL DEFAULT 'kg',
    max_kg_per_acre    INT  NOT NULL CHECK (max_kg_per_acre > 0)
);
INSERT INTO crops(code, name, max_kg_per_acre) VALUES
    ('maize', 'Maize', 3200), ('beans', 'Beans', 1200), ('coffee', 'Coffee cherry', 6000),
    ('tea', 'Tea leaf', 12000), ('wheat', 'Wheat', 3000), ('sorghum', 'Sorghum', 2500),
    ('potatoes', 'Potatoes', 16000), ('avocado', 'Avocado', 12000)
ON CONFLICT (code) DO NOTHING;

CREATE TABLE IF NOT EXISTS centres (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 2 AND 120),
    location    TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (coop_id, name)
);

-- Cooperative staff who sign in to the field app with their phone
CREATE TABLE IF NOT EXISTS staff (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    centre_id   UUID REFERENCES centres(id),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 2 AND 120),
    phone       TEXT NOT NULL CHECK (phone ~ '^[0-9]{9,15}$'),
    role        TEXT NOT NULL CHECK (role IN ('manager','clerk')),
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (coop_id, phone)
);

CREATE TABLE IF NOT EXISTS farmers (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coop_id       UUID NOT NULL REFERENCES cooperatives(id),
    member_no     INT  NOT NULL,
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 2 AND 120),
    phone         TEXT NOT NULL CHECK (phone ~ '^[0-9]{9,15}$'),
    village       TEXT NOT NULL DEFAULT '',
    acres         NUMERIC(7,2) NOT NULL CHECK (acres > 0 AND acres <= 1000),
    lat           DOUBLE PRECISION,
    lng           DOUBLE PRECISION,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','held','rejected','inactive')),
    ai_level      TEXT NOT NULL DEFAULT 'low',
    ai_reasons    JSONB NOT NULL DEFAULT '[]'::jsonb,
    registered_by UUID REFERENCES staff(id),
    decided_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (coop_id, phone),
    UNIQUE (coop_id, member_no)
);

CREATE TABLE IF NOT EXISTS prices (
    id             BIGSERIAL PRIMARY KEY,
    coop_id        UUID NOT NULL REFERENCES cooperatives(id),
    crop           TEXT NOT NULL REFERENCES crops(code),
    grade          TEXT NOT NULL CHECK (grade IN ('1','2')),
    price_per_kg   NUMERIC(10,2) NOT NULL CHECK (price_per_kg > 0),
    set_by         TEXT NOT NULL,
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS prices_lookup_idx ON prices(coop_id, crop, grade, effective_from DESC);

CREATE TABLE IF NOT EXISTS lots (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    code        TEXT NOT NULL UNIQUE,                 -- public code on the QR proof page
    crop        TEXT NOT NULL REFERENCES crops(code),
    buyer       TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS payruns (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coop_id      UUID NOT NULL REFERENCES cooperatives(id),
    created_by   UUID NOT NULL REFERENCES staff(id),
    approved_by  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','approved','cancelled')),
    total        BIGINT NOT NULL DEFAULT 0,
    farmers      INT NOT NULL DEFAULT 0,
    deliveries   INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS deliveries (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    coop_id       UUID NOT NULL REFERENCES cooperatives(id),
    centre_id     UUID REFERENCES centres(id),
    farmer_id     UUID NOT NULL REFERENCES farmers(id),
    clerk_id      UUID NOT NULL REFERENCES staff(id),
    crop          TEXT NOT NULL REFERENCES crops(code),
    grade         TEXT NOT NULL CHECK (grade IN ('1','2','reject')),
    weight_kg     NUMERIC(9,1) NOT NULL CHECK (weight_kg > 0 AND weight_kg <= 10000),
    moisture      NUMERIC(4,1) CHECK (moisture IS NULL OR (moisture >= 0 AND moisture <= 60)),
    price_per_kg  NUMERIC(10,2) NOT NULL DEFAULT 0,
    amount        BIGINT NOT NULL DEFAULT 0 CHECK (amount >= 0),
    status        TEXT NOT NULL DEFAULT 'recorded' CHECK (status IN ('recorded','held','voided')),
    ai_score      DOUBLE PRECISION NOT NULL DEFAULT 0,
    ai_level      TEXT NOT NULL DEFAULT 'low',
    ai_reasons    JSONB NOT NULL DEFAULT '[]'::jsonb,
    reviewed_by   TEXT NOT NULL DEFAULT '',
    lot_id        UUID REFERENCES lots(id),
    payrun_id     UUID REFERENCES payruns(id),
    ledger_seq    BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deliveries_farmer_idx ON deliveries(farmer_id, created_at);
CREATE INDEX IF NOT EXISTS deliveries_clerk_idx ON deliveries(clerk_id, created_at);
CREATE INDEX IF NOT EXISTS deliveries_unpaid_idx ON deliveries(coop_id) WHERE payrun_id IS NULL AND status = 'recorded';

-- Append-only, hash-chained ledger, one chain per cooperative. Every delivery, correction and
-- payment is a line; weight and grade are inside the hashed reference, so they cannot change quietly.
CREATE TABLE IF NOT EXISTS ledger_entries (
    id          BIGSERIAL PRIMARY KEY,
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    seq         BIGINT NOT NULL,
    entry_type  TEXT NOT NULL CHECK (entry_type IN ('delivery','void','payment','return','fund','fee')),
    farmer_id   UUID REFERENCES farmers(id),
    amount      BIGINT NOT NULL,
    reference   TEXT NOT NULL DEFAULT '',
    prev_hash   TEXT NOT NULL,
    hash        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    UNIQUE (coop_id, seq)
);
CREATE OR REPLACE FUNCTION ledger_is_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ledger_no_update ON ledger_entries;
CREATE TRIGGER ledger_no_update BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_is_append_only();

CREATE TABLE IF NOT EXISTS anchors (
    id          BIGSERIAL PRIMARY KEY,
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    entries     BIGINT NOT NULL,
    head        TEXT NOT NULL,
    tx_hash     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- sign-in, sessions, audit ----------
CREATE TABLE IF NOT EXISTS otp_codes (
    id          BIGSERIAL PRIMARY KEY,
    phone       TEXT NOT NULL,
    code_hash   TEXT NOT NULL,
    attempts    INT  NOT NULL DEFAULT 0,
    used        BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS otp_codes_phone_idx ON otp_codes(phone, created_at DESC);

CREATE TABLE IF NOT EXISTS phone_sessions (
    token_hash   TEXT PRIMARY KEY,
    phone        TEXT NOT NULL,
    device       TEXT NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS admins (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username        TEXT NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9._-]{3,40}$'),
    password_hash   TEXT NOT NULL,             -- Argon2id
    totp_secret     TEXT NOT NULL,             -- base32, for authenticator apps
    totp_last_step  BIGINT NOT NULL DEFAULT 0, -- blocks reuse of the same code
    failed_attempts INT NOT NULL DEFAULT 0,
    locked_until    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at   TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS admin_sessions (
    token_hash  TEXT PRIMARY KEY,
    admin_id    UUID NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGSERIAL PRIMARY KEY,
    actor_type  TEXT NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    detail      JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip          TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS audit_no_update ON audit_log;
CREATE TRIGGER audit_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION ledger_is_append_only();

CREATE TABLE IF NOT EXISTS app_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
