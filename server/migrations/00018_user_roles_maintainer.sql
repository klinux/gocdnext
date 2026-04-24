-- +goose Up
-- +goose StatementBegin

-- Rename the middle role from the ambiguous "user" to the more
-- explicit "maintainer" and widen the accepted enum to the
-- three-role hierarchy RBAC layer now wires up (admin ≥
-- maintainer ≥ viewer). Existing rows flip to maintainer so
-- nobody loses access — current "users" had write permissions
-- equivalent to the new maintainer tier.

ALTER TABLE users DROP CONSTRAINT users_role_check;

UPDATE users SET role = 'maintainer' WHERE role = 'user';

ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'maintainer', 'viewer'));

-- Default changes from 'user' to 'viewer' for freshly-provisioned
-- rows. The OAuth + local login flows already pick explicit
-- roles at signup time (admin allow-list vs the initial role on
-- local-user create), so this just makes the fallback conservative
-- — a row inserted via SQL or a forgotten code path defaults to
-- read-only instead of write-capable.
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'viewer';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE users ALTER COLUMN role SET DEFAULT 'user';
ALTER TABLE users DROP CONSTRAINT users_role_check;
UPDATE users SET role = 'user' WHERE role = 'maintainer';
ALTER TABLE users
    ADD CONSTRAINT users_role_check CHECK (role IN ('admin', 'user', 'viewer'));

-- +goose StatementEnd
