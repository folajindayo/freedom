-- Extensions.
--
-- Created here rather than in a migration because installing an extension
-- requires privileges the application role deliberately does not have. This
-- file runs once, as the database owner; migrations run as freedom_scheme.
--
-- btree_gist lets an exclusion constraint mix an equality test on text with a
-- range overlap test — "one open period per instrument", and the same for BIN
-- ranges on the card side.
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Roles and grants. Run once per database, before any migration.
--
-- The two roles are the mechanism behind participant isolation: RLS is worth
-- nothing if every binary connects as the same role. See docs/SECURITY.md §7.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'freedom_scheme') THEN
    -- The switch, clearing and the auction engine are inherently
    -- cross-participant and cannot work through a policy that hides half the
    -- network from them.
    CREATE ROLE freedom_scheme LOGIN PASSWORD 'freedom' BYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'freedom_participant') THEN
    -- The acquirer API and the member portal. RLS enforced.
    CREATE ROLE freedom_participant LOGIN PASSWORD 'freedom';
  END IF;
END
$$;

GRANT ALL   ON SCHEMA public TO freedom_scheme;
GRANT USAGE ON SCHEMA public TO freedom_participant;

ALTER DEFAULT PRIVILEGES FOR ROLE freedom_scheme IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE ON TABLES TO freedom_participant;
ALTER DEFAULT PRIVILEGES FOR ROLE freedom_scheme IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO freedom_participant;
