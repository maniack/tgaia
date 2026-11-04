ALTER TABLE runtime_settings RENAME TO runtime_settings_unscoped;
CREATE TABLE runtime_settings (
 scope TEXT NOT NULL CHECK(scope IN ('shared','api','worker')),
 name TEXT NOT NULL,
 value TEXT NOT NULL,
 secret BOOLEAN NOT NULL,
 version BIGINT NOT NULL DEFAULT 1,
 PRIMARY KEY(scope,name)
);
-- Process identity and listeners are deployment-local. Old unscoped values cannot
-- be assigned safely to API versus worker replicas, so operators must reapply them
-- in the appropriate scope (role itself remains bootstrap-only).
INSERT INTO runtime_settings(scope,name,value,secret,version)
 SELECT 'shared',name,value,secret,version FROM runtime_settings_unscoped
 WHERE name NOT IN ('role','listen','public_url');
DROP TABLE runtime_settings_unscoped;
