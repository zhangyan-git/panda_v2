CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS merchants (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name TEXT NOT NULL, contact_name TEXT NOT NULL DEFAULT '', contact_phone TEXT NOT NULL DEFAULT '', business_license TEXT NOT NULL DEFAULT '', address TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')), expire_date TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS stores (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(), merchant_id UUID NOT NULL REFERENCES merchants(id) ON DELETE CASCADE, brand_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, logo TEXT NOT NULL DEFAULT '', phone TEXT NOT NULL DEFAULT '', province TEXT NOT NULL DEFAULT '', city TEXT NOT NULL DEFAULT '', district TEXT NOT NULL DEFAULT '', address TEXT NOT NULL DEFAULT '', business_hours TEXT NOT NULL DEFAULT '', longitude DOUBLE PRECISION NOT NULL DEFAULT 0, latitude DOUBLE PRECISION NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')), audit_status TEXT NOT NULL DEFAULT 'pending' CHECK (audit_status IN ('pending','approved','rejected')), audit_remark TEXT NOT NULL DEFAULT '', visible BOOLEAN NOT NULL DEFAULT TRUE, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS stores_merchant_id_idx ON stores(merchant_id);
CREATE TABLE IF NOT EXISTS merchant_accounts (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(), merchant_id UUID NOT NULL REFERENCES merchants(id) ON DELETE CASCADE, account_id TEXT NOT NULL UNIQUE, real_name TEXT NOT NULL DEFAULT '', is_admin BOOLEAN NOT NULL DEFAULT FALSE, permission_type TEXT NOT NULL CHECK (permission_type IN ('all','brand','store')), brand_ids TEXT[] NOT NULL DEFAULT '{}', store_ids TEXT[] NOT NULL DEFAULT '{}', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS store_audits (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(), store_id UUID NOT NULL REFERENCES stores(id) ON DELETE CASCADE, type TEXT NOT NULL CHECK (type IN ('create','update')), status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')), new_data BYTEA, old_data BYTEA, submitted_by TEXT NOT NULL, audited_by TEXT NOT NULL DEFAULT '', audit_remark TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS store_audits_store_id_idx ON store_audits(store_id);
