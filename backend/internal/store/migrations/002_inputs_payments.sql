-- AgriProof schema v2: fertilizer and other farm inputs, farmer payments, money check

-- ---------- inputs (fertilizer, seed) ----------
CREATE TABLE IF NOT EXISTS suppliers (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 2 AND 160),
    kind         TEXT NOT NULL CHECK (kind IN ('manufacturer','importer','distributor')),
    reg_no       TEXT NOT NULL DEFAULT '',        -- business registration number
    kebs_permit  TEXT NOT NULL DEFAULT '',        -- KEBS standardization mark permit / certificate
    phone        TEXT NOT NULL DEFAULT '',
    county       TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','verified','suspended')),
    notes        TEXT NOT NULL DEFAULT '',
    decided_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS input_batches (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    supplier_id     UUID NOT NULL REFERENCES suppliers(id),
    product         TEXT NOT NULL CHECK (length(product) BETWEEN 2 AND 120),   -- e.g. DAP, CAN, NPK 23-23-0, hybrid maize seed
    input_type      TEXT NOT NULL DEFAULT 'fertilizer' CHECK (input_type IN ('fertilizer','seed','agrochemical')),
    stated_content  TEXT NOT NULL DEFAULT '',      -- what the bag says, e.g. N 18% P2O5 46%
    bag_kg          NUMERIC(6,1) NOT NULL CHECK (bag_kg > 0),
    batch_no        TEXT NOT NULL CHECK (length(batch_no) BETWEEN 1 AND 60),
    made_on         DATE,
    expires_on      DATE,
    bags            INT NOT NULL CHECK (bags > 0 AND bags <= 200000),
    lab_result      TEXT NOT NULL DEFAULT 'untested' CHECK (lab_result IN ('untested','pass','fail')),
    lab_content     TEXT NOT NULL DEFAULT '',
    lab_ref         TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','recalled')),
    recall_reason   TEXT NOT NULL DEFAULT '',
    codes_issued    BOOLEAN NOT NULL DEFAULT FALSE,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (supplier_id, batch_no)
);

-- One code per bag. Only a keyed hash is stored; the printable codes are shown once, at creation.
CREATE TABLE IF NOT EXISTS bag_codes (
    id                   BIGSERIAL PRIMARY KEY,
    batch_id             UUID NOT NULL REFERENCES input_batches(id),
    serial               INT NOT NULL,
    code_hash            TEXT NOT NULL UNIQUE,
    first_checked_at     TIMESTAMPTZ,
    first_checked_phone  TEXT NOT NULL DEFAULT '',
    checks               INT NOT NULL DEFAULT 0,
    issued_to            UUID REFERENCES farmers(id),
    UNIQUE (batch_id, serial)
);

CREATE TABLE IF NOT EXISTS code_checks (
    id          BIGSERIAL PRIMARY KEY,
    bag_id      BIGINT REFERENCES bag_codes(id),
    batch_id    UUID REFERENCES input_batches(id),
    phone       TEXT NOT NULL DEFAULT '',
    channel     TEXT NOT NULL CHECK (channel IN ('sms','web','app')),
    ip          TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL CHECK (result IN ('genuine','repeat','not_found','recalled')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS code_checks_time_idx ON code_checks(created_at);
CREATE INDEX IF NOT EXISTS code_checks_phone_idx ON code_checks(phone, created_at);

-- Where bags went: supplier -> dealer or cooperative store
CREATE TABLE IF NOT EXISTS shipments (
    id          BIGSERIAL PRIMARY KEY,
    batch_id    UUID NOT NULL REFERENCES input_batches(id),
    to_coop_id  UUID REFERENCES cooperatives(id),
    to_name     TEXT NOT NULL,                     -- dealer or store name
    bags        INT NOT NULL CHECK (bags > 0),
    recorded_by TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Fertilizer or seed handed to a farmer by the cooperative
CREATE TABLE IF NOT EXISTS input_issues (
    id          BIGSERIAL PRIMARY KEY,
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    farmer_id   UUID NOT NULL REFERENCES farmers(id),
    batch_id    UUID NOT NULL REFERENCES input_batches(id),
    bag_id      BIGINT REFERENCES bag_codes(id),
    bags        NUMERIC(6,1) NOT NULL CHECK (bags > 0),
    issued_by   UUID NOT NULL REFERENCES staff(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS input_issues_batch_idx ON input_issues(batch_id);

-- Farmers report a bad bag ("did not work", "looked wrong")
CREATE TABLE IF NOT EXISTS input_complaints (
    id          BIGSERIAL PRIMARY KEY,
    batch_id    UUID REFERENCES input_batches(id),
    farmer_id   UUID REFERENCES farmers(id),
    phone       TEXT NOT NULL,
    problem     TEXT NOT NULL CHECK (length(problem) BETWEEN 3 AND 500),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- money ----------
-- Cooperative money put into the payment account (confirmed by the payment company)
CREATE TABLE IF NOT EXISTS fundings (
    id          TEXT PRIMARY KEY,                  -- provider request id
    coop_id     UUID NOT NULL REFERENCES cooperatives(id),
    phone       TEXT NOT NULL,
    amount      BIGINT NOT NULL CHECK (amount > 0),
    net_amount  BIGINT,
    fee         BIGINT NOT NULL DEFAULT 0,
    provider    TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','completed','failed')),
    result_desc TEXT NOT NULL DEFAULT '',
    receipt     TEXT NOT NULL DEFAULT '',
    started_by  TEXT NOT NULL,
    checked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One payout per farmer per approved pay run. Never sent twice: unclear outcomes stop at 'unknown'.
CREATE TABLE IF NOT EXISTS payouts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payrun_id       UUID NOT NULL REFERENCES payruns(id),
    coop_id         UUID NOT NULL REFERENCES cooperatives(id),
    farmer_id       UUID NOT NULL REFERENCES farmers(id),
    phone           TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    amount          BIGINT NOT NULL CHECK (amount > 0),
    provider        TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued','sending','sent','paid','failed','unknown','manual','returned')),
    provider_ref    TEXT NOT NULL DEFAULT '',
    receipt         TEXT NOT NULL DEFAULT '',
    fee             BIGINT NOT NULL DEFAULT 0,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_by     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (payrun_id, farmer_id)
);
CREATE INDEX IF NOT EXISTS payouts_status_idx ON payouts(status, next_attempt_at);

CREATE TABLE IF NOT EXISTS reconciliations (
    id               BIGSERIAL PRIMARY KEY,
    provider         TEXT NOT NULL,
    live             BOOLEAN NOT NULL,
    account_balance  BIGINT,
    account_source   TEXT NOT NULL,
    expected_account BIGINT NOT NULL,
    difference       BIGINT,
    status           TEXT NOT NULL CHECK (status IN ('ok','mismatch','unavailable')),
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
