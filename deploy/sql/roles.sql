-- Database roles for floorrules (day-2 review M5). Run once per database, as
-- an admin connected to that database, after creating two login roles:
--
--   floorsvc_migrator  owns the schema; used only by the migrate Job
--                      (`floorsvc migrate`).
--   floorsvc_app       the service's runtime role: reads and writes rows,
--                      cannot create, alter or drop anything.
--
-- On Cloud SQL create the login roles with `gcloud sql users create` (or
-- google_sql_user) and put each DSN in its own Secret Manager secret.
-- internal/store/pgstore TestRoles_RuntimeRoleHasNoDDL runs this file
-- against Postgres 16 and proves the split.

-- Only the migrator may create objects in the schema. (Postgres 15+ already
-- revokes CREATE from PUBLIC; this makes it explicit on older clusters.)
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO floorsvc_migrator;
GRANT USAGE ON SCHEMA public TO floorsvc_app;

-- Everything the migrator creates from now on is usable by the runtime role
-- for DML only.
ALTER DEFAULT PRIVILEGES FOR ROLE floorsvc_migrator IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO floorsvc_app;
ALTER DEFAULT PRIVILEGES FOR ROLE floorsvc_migrator IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO floorsvc_app;
