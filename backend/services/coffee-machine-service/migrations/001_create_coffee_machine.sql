CREATE TABLE manufacturers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL CHECK (length(trim(name)) > 0), contact_name TEXT, contact_phone TEXT,
    code TEXT, merchant_id TEXT, api_base_url TEXT, test_api_base_url TEXT,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    username TEXT, secret TEXT, token TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE devices (
    id TEXT PRIMARY KEY, manufacturer_id TEXT NOT NULL REFERENCES manufacturers(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (length(trim(name)) > 0), serial_number TEXT, location TEXT,
    serial_unique TEXT, device_name TEXT, manufacturer_code TEXT, store_id TEXT, store_name TEXT,
    online BOOLEAN NOT NULL DEFAULT FALSE, version TEXT, address TEXT, error TEXT,
    last_activity_at TIMESTAMPTZ, display_config JSONB, payment_config JSONB,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT devices_serial_number_unique UNIQUE (serial_number)
);
CREATE UNIQUE INDEX devices_serial_unique_unique_idx ON devices (serial_unique) WHERE serial_unique IS NOT NULL;
CREATE TABLE drinks (
    id TEXT PRIMARY KEY, name TEXT NOT NULL CHECK (length(trim(name)) > 0), description TEXT,
    origin_id TEXT, product_num TEXT, en_name TEXT, price BIGINT NOT NULL DEFAULT 0 CHECK (price >= 0),
    vip_price BIGINT NOT NULL DEFAULT 0 CHECK (vip_price >= 0), pickup_code_price BIGINT NOT NULL DEFAULT 0 CHECK (pickup_code_price >= 0),
    image TEXT, sort INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX drinks_origin_id_unique_idx ON drinks (origin_id) WHERE origin_id IS NOT NULL;
CREATE TABLE device_drinks (
    device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, drink_id TEXT REFERENCES drinks(id) ON DELETE CASCADE,
    origin_id TEXT, relation_key TEXT GENERATED ALWAYS AS (CASE WHEN NULLIF(trim(origin_id), '') IS NOT NULL THEN 'origin:' || NULLIF(trim(origin_id), '') ELSE 'drink:' || drink_id END) STORED,
    enabled BOOLEAN NOT NULL DEFAULT TRUE, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT device_drinks_identifier_check CHECK (NULLIF(trim(origin_id), '') IS NOT NULL OR NULLIF(trim(drink_id), '') IS NOT NULL),
    CONSTRAINT device_drinks_relation_unique UNIQUE (device_id, relation_key)
);
CREATE INDEX devices_manufacturer_id_idx ON devices (manufacturer_id); CREATE INDEX devices_status_idx ON devices (status);
CREATE INDEX drinks_status_idx ON drinks (status); CREATE INDEX device_drinks_drink_id_idx ON device_drinks (drink_id);
