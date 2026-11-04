ALTER TABLE seen_users ADD COLUMN role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('reader','user'));
ALTER TABLE seen_users ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
