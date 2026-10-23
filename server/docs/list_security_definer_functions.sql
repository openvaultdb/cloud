-- Lists SECURITY DEFINER functions the DemoDB reader role may EXECUTE, in any
-- schema outside pg_catalog/information_schema (including `public`).
-- openvaultdb-go v0.25.2 refuses to mount a role that can execute one in an
-- exposed schema, and PostgreSQL grants EXECUTE to PUBLIC by default, so run
-- this against each edition's database BEFORE activating the mounts.
--
-- Run as an administrative role, one database at a time (no secrets in this file):
--   psql "$ADMIN_DSN" -v reader=chinook_reader -f server/docs/list_security_definer_functions.sql
--
-- An empty result is what you want. For each row, run the revoke_sql value, or
-- revoke from the specific role if the grant is not through PUBLIC.
SELECT n.nspname                                            AS schema_name,
       p.proname                                            AS function_name,
       pg_catalog.pg_get_function_identity_arguments(p.oid) AS arguments,
       pg_catalog.pg_get_userbyid(p.proowner)               AS owner,
       pg_catalog.has_function_privilege('public', p.oid, 'EXECUTE') AS public_can_execute,
       format('REVOKE EXECUTE ON FUNCTION %I.%I(%s) FROM PUBLIC;',
              n.nspname, p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid)) AS revoke_sql
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE p.prosecdef
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_%'
  AND pg_catalog.has_function_privilege(:'reader', p.oid, 'EXECUTE')
ORDER BY n.nspname, p.proname;
