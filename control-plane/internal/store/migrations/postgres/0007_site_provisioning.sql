-- Site provisioning (#27). Keep in sync with
-- migrations/sqlite/0007_site_provisioning.sql.

-- Why the site has its status, e.g. the error that made provisioning fail.
ALTER TABLE sites ADD COLUMN status_message TEXT NOT NULL DEFAULT '';

-- The initial WordPress administrator. The password is generated at creation
-- and stored as a secret that the API hands out once and then deletes, so
-- admin_password_secret_ref is empty once it has been retrieved (§5.1).
ALTER TABLE sites ADD COLUMN admin_username TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN admin_email TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN admin_password_secret_ref TEXT NOT NULL DEFAULT '';

CREATE INDEX sites_status ON sites (status);
