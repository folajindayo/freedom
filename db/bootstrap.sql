-- Roles and grants. Run once per database, before any migration.
--
-- The two roles are the mechanism behind participant isolation: RLS is worth
-- nothing if every binary connects as the same role. See docs/SECURITY.md §7.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'oja_scheme') THEN
    -- The switch, clearing and the auction engine are inherently
    -- cross-participant and cannot work through a policy that hides half the
    -- network from them.
    CREATE ROLE oja_scheme LOGIN PASSWORD 'oja' BYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'oja_participant') THEN
    -- The acquirer API and the member portal. RLS enforced.
    CREATE ROLE oja_participant LOGIN PASSWORD 'oja';
  END IF;
END
$$;

GRANT ALL   ON SCHEMA public TO oja_scheme;
GRANT USAGE ON SCHEMA public TO oja_participant;

ALTER DEFAULT PRIVILEGES FOR ROLE oja_scheme IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE ON TABLES TO oja_participant;
ALTER DEFAULT PRIVILEGES FOR ROLE oja_scheme IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO oja_participant;
