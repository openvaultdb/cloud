package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

// demoPostgresInsecureTLSEnv lets the local real-PostgreSQL harness use
// sslmode=disable. It is honoured only when the DSN's single host is a loopback
// address, so it cannot weaken a Neon (or any other remote) connection, and it is
// never set by the deploy workflow.
const demoPostgresInsecureTLSEnv = "OVDB_PG_INSECURE_TLS_FOR_TESTS"

// demoPostgresMaxConnectTimeout bounds the connect_timeout every DSN must set, so
// a dial that never answers ends and a background mount attempt always finishes.
const demoPostgresMaxConnectTimeout = 30 * time.Second

// demoPostgresStatementTimeout is the statement_timeout every mounted session
// starts with. The library (openvaultdb-go >= v0.25.2) writes it into the
// mount's connection string, after any statement_timeout the DSN sets, so it
// wins over the DSN and over a role default.
const demoPostgresStatementTimeout = 15 * time.Second

// demoPostgresDiscoveryTimeout bounds the library's catalogue read of one mount;
// defaultDemoPostgresMountTiming's Attempt leaves room for it.
const demoPostgresDiscoveryTimeout = 30 * time.Second

// applyLibraryMountHardening sets the library's per-mount hardening options
// (openvaultdb-go v0.25.2):
//
//   - RequireReadOnlyNativePostgresRole: the library refuses a role that is a
//     superuser, owns or may write an exposed relation, owns a schema, relation or
//     function, may create in an exposed schema, may use or update a sequence, or
//     may execute a SECURITY DEFINER function in an exposed schema. It overlaps
//     with checkDemoPostgresSource, which stays: each refuses things the other
//     does not (see README "Mount hardening and operator requirements").
//   - NativePostgresStatementTimeout: every session starts with this
//     statement_timeout. It replaces this service's former requirement that the
//     role or DSN carry one.
//   - AllowInsecureNativePostgresTransport stays false: a remote host needs
//     verified TLS. This service's own DSN policy (verify-full only) is stricter,
//     and OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT is refused by it.
//
// Every read additionally runs in a BEGIN READ ONLY transaction (library
// default for native mounts), on top of default_transaction_read_only=on.
// mount.All / Spec.Optional is not used: demoPostgresMounter already isolates
// each mount and, unlike mount.All, retries a failed one with backoff.
func applyLibraryMountHardening(options *mount.Options) {
	options.RequireReadOnlyNativePostgresRole = true
	options.NativePostgresStatementTimeout = demoPostgresStatementTimeout
	options.NativePostgresDiscoveryTimeout = demoPostgresDiscoveryTimeout
	options.AllowInsecureNativePostgresTransport = false
}

// The refusal errors are fixed strings: they are logged, and their reason codes
// (demoPostgresReasonCode) are served by the status route, so they must never
// carry a DSN, host, user or password.
var (
	errDemoPostgresDSNInvalid      = errors.New("DSN is not a valid PostgreSQL connection string")
	errDemoPostgresDSNAmbiguous    = errors.New("DSN must name exactly one host and set each connection key once (no service files)")
	errDemoPostgresEnv             = errors.New("PG* connection environment variables (PGSSLMODE, PGHOST, PGOPTIONS, PGSERVICE, ...) must not be set")
	errDemoPostgresInsecureEnv     = errors.New(mount.InsecureNativePostgresTransportEnv + " must not be set: it would let the library skip TLS verification")
	errDemoPostgresTLS             = errors.New("DSN must set sslmode=verify-full")
	errDemoPostgresInsecureHost    = errors.New(demoPostgresInsecureTLSEnv + " is honoured only for a single loopback host")
	errDemoPostgresOptionsParam    = errors.New("DSN must not set the PostgreSQL options parameter")
	errDemoPostgresRuntimeParam    = errors.New("DSN sets a session parameter other than statement_timeout, lock_timeout, idle_in_transaction_session_timeout or application_name")
	errDemoPostgresConnectTimeout  = errors.New("DSN must set connect_timeout between 1 and 30 seconds")
	errDemoPostgresOwnerRole       = errors.New("role is an owner or privileged role; use a dedicated read-only role")
	errDemoPostgresPrivilegedGrant = errors.New("role is a member of a privileged, owning or server-access role; revoke the membership")
	errDemoPostgresRoleSwitched    = errors.New("session_user and current_user differ")
	errDemoPostgresWritableRole    = errors.New("role can write, create or own objects; grant SELECT and USAGE only")
	errDemoPostgresSchemaScope     = errors.New("role can read relations outside its own schema")
	errDemoPostgresUnreachable     = errors.New("could not connect to PostgreSQL or run the role self-check")
)

// demoPostgresConnectionEnv lists every environment variable pgx reads as a
// connection default. They would apply to anything the DSN leaves unset, so none
// may be set in this process.
var demoPostgresConnectionEnv = []string{
	"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGAPPNAME",
	"PGCONNECT_TIMEOUT", "PGSSLMODE", "PGSSLKEY", "PGSSLCERT", "PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD",
	"PGSSLNEGOTIATION", "PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE", "PGTZ", "PGOPTIONS",
	"PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION", "PGCHANNELBINDING", "PGREQUIREAUTH",
}

// demoPostgresAllowedRuntimeParams are the only session parameters a DSN may
// send at startup (default_transaction_read_only only as "on"; the mount forces it).
var demoPostgresAllowedRuntimeParams = map[string]bool{
	"statement_timeout": true, "lock_timeout": true, "idle_in_transaction_session_timeout": true, "application_name": true,
}

// demoPostgresTLSAllowedInsecurely reports whether the test-only TLS bypass is set.
func demoPostgresTLSAllowedInsecurely() bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(demoPostgresInsecureTLSEnv)))
	return err == nil && value
}

// validateDemoPostgresDSN refuses a DSN that could connect without verifying the
// server certificate, or whose meaning depends on anything but its own text. The
// explicit keys are checked first, then the configuration pgx actually derives
// from the DSN is checked, so a key pgx resolves differently cannot slip past.
func validateDemoPostgresDSN(dsn string, allowInsecureLoopback bool) error {
	return validateDemoPostgresDSNWithEnv(dsn, allowInsecureLoopback, os.LookupEnv)
}

func validateDemoPostgresDSNWithEnv(dsn string, allowInsecureLoopback bool, lookupEnv func(string) (string, bool)) error {
	for _, name := range demoPostgresConnectionEnv {
		if value, set := lookupEnv(name); set && value != "" {
			return errDemoPostgresEnv
		}
	}
	// The library honours this process-wide opt-out of verified TLS; nothing in
	// this service may set it, so its presence is a configuration error.
	if value, set := lookupEnv(mount.InsecureNativePostgresTransportEnv); set && value != "" {
		return errDemoPostgresInsecureEnv
	}
	params, hosts, err := demoPostgresDSNParams(dsn)
	if err != nil {
		if errors.Is(err, errDemoPostgresDSNAmbiguous) {
			return errDemoPostgresDSNAmbiguous
		}
		return errDemoPostgresDSNInvalid
	}
	if len(hosts) != 1 || hosts[0] == "" || strings.HasPrefix(hosts[0], "/") || params["service"] != "" || params["servicefile"] != "" || params["passfile"] != "" {
		return errDemoPostgresDSNAmbiguous
	}
	if _, set := params["options"]; set {
		return errDemoPostgresOptionsParam
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errDemoPostgresDSNInvalid
	}
	if len(config.Fallbacks) > 0 && params["sslmode"] == "verify-full" {
		return errDemoPostgresDSNAmbiguous
	}
	for name, value := range config.RuntimeParams {
		if name == "options" {
			return errDemoPostgresOptionsParam
		}
		if name == "default_transaction_read_only" && strings.EqualFold(value, "on") {
			continue
		}
		if !demoPostgresAllowedRuntimeParams[name] {
			return errDemoPostgresRuntimeParam
		}
	}
	if config.ConnectTimeout <= 0 || config.ConnectTimeout > demoPostgresMaxConnectTimeout {
		return errDemoPostgresConnectTimeout
	}
	if params["sslmode"] == "verify-full" {
		tlsConfig := config.TLSConfig
		if tlsConfig == nil || tlsConfig.InsecureSkipVerify || tlsConfig.ServerName == "" || tlsConfig.VerifyPeerCertificate != nil {
			return errDemoPostgresTLS
		}
		return nil
	}
	if !allowInsecureLoopback {
		return errDemoPostgresTLS
	}
	if !loopbackHost(hosts[0]) || !loopbackHost(config.Host) {
		return errDemoPostgresInsecureHost
	}
	for _, fallback := range config.Fallbacks {
		if !loopbackHost(fallback.Host) {
			return errDemoPostgresInsecureHost
		}
	}
	return nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// demoPostgresNeonPooler reports whether the host is a Neon pooled endpoint
// (…-pooler.…neon.tech). The pooler is not proven to keep the
// default_transaction_read_only startup parameter, so it is warned about.
func demoPostgresNeonPooler(dsn string) bool {
	_, hosts, err := demoPostgresDSNParams(dsn)
	if err != nil {
		return false
	}
	for _, host := range hosts {
		label, _, _ := strings.Cut(host, ".")
		if strings.HasSuffix(label, "-pooler") {
			return true
		}
	}
	return false
}

// demoPostgresDSNParams returns the explicit connection parameters and hosts of a
// URL or keyword/value DSN. A key given twice (including a URL host and a host
// query parameter) is errDemoPostgresDSNAmbiguous: libpq-style parsers disagree
// on which occurrence wins. Each comma-separated host is returned separately.
func demoPostgresDSNParams(dsn string) (map[string]string, []string, error) {
	params := map[string]string{}
	var hosts []string
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return nil, nil, err
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return nil, nil, err
		}
		for key, values := range query {
			if len(values) != 1 {
				return nil, nil, errDemoPostgresDSNAmbiguous
			}
			params[key] = values[0]
		}
		if parsed.Host != "" {
			if _, set := params["host"]; set {
				return nil, nil, errDemoPostgresDSNAmbiguous
			}
			for _, hostPort := range strings.Split(parsed.Host, ",") {
				host := hostPort
				if h, _, err := net.SplitHostPort(hostPort); err == nil {
					host = h
				}
				hosts = append(hosts, host)
			}
		}
	} else {
		for rest := strings.TrimSpace(dsn); rest != ""; rest = strings.TrimSpace(rest) {
			key, value, ok := strings.Cut(rest, "=")
			if !ok {
				return nil, nil, errors.New("keyword without value")
			}
			key = strings.TrimSpace(key)
			value = strings.TrimLeft(value, " \t")
			var parsedValue string
			if strings.HasPrefix(value, "'") {
				var b strings.Builder
				i := 1
				for ; i < len(value); i++ {
					if value[i] == '\\' && i+1 < len(value) {
						i++
						b.WriteByte(value[i])
						continue
					}
					if value[i] == '\'' {
						break
					}
					b.WriteByte(value[i])
				}
				if i >= len(value) {
					return nil, nil, errors.New("unterminated quoted value")
				}
				parsedValue = b.String()
				rest = value[i+1:]
			} else {
				end := strings.IndexAny(value, " \t")
				if end < 0 {
					end = len(value)
				}
				parsedValue = value[:end]
				rest = value[end:]
			}
			if _, seen := params[key]; seen {
				return nil, nil, errDemoPostgresDSNAmbiguous
			}
			params[key] = parsedValue
		}
	}
	if value, set := params["host"]; set {
		hosts = append(hosts, strings.Split(value, ",")...)
	}
	if value, set := params["hostaddr"]; set {
		if len(hosts) > 0 {
			return nil, nil, errDemoPostgresDSNAmbiguous
		}
		hosts = append(hosts, strings.Split(value, ",")...)
	}
	return params, hosts, nil
}

// demoPostgresRoleFacts is what the role self-check reads from PostgreSQL.
type demoPostgresRoleFacts struct {
	Role             string
	SessionRole      string
	Privileged       bool     // superuser, createrole, createdb, replication or bypassrls
	PrivilegedMember bool     // member (with or without INHERIT/SET) of a privileged, owning or server-access role
	CanWrite         bool     // owns, or may write/alter, any non-system relation or sequence
	CanCreate        bool     // may create in any non-system schema, or create schemas in the database
	ReadableSchemas  []string // non-system schemas holding at least one SELECT-able relation
}

// demoPostgresRoleFactsSQL reads the facts. Membership uses pg_has_role(...,
// 'MEMBER'), which holds for any direct or indirect grant whatever its INHERIT
// and SET options, because a member that cannot inherit may still SET ROLE.
const demoPostgresRoleFactsSQL = `
SELECT
  current_user::text,
  session_user::text,
  (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls),
  EXISTS (
    SELECT 1 FROM pg_catalog.pg_roles m
    WHERE m.oid <> r.oid AND pg_catalog.pg_has_role(r.oid, m.oid, 'MEMBER')
      AND (m.rolsuper OR m.rolcreaterole OR m.rolcreatedb OR m.rolreplication OR m.rolbypassrls
        OR m.rolname IN ('pg_write_all_data', 'pg_read_all_data', 'pg_execute_server_program', 'pg_read_server_files',
                         'pg_write_server_files', 'pg_database_owner', 'pg_signal_backend', 'pg_create_subscription',
                         'pg_checkpoint', 'pg_maintain', 'pg_use_reserved_connections', 'pg_signal_autovacuum_worker')
        OR EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                   WHERE c.relowner = m.oid AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%')
        OR EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n
                   WHERE n.nspowner = m.oid AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%')
        OR EXISTS (SELECT 1 FROM pg_catalog.pg_database d WHERE d.datname = current_database() AND d.datdba = m.oid))
  ),
  EXISTS (
    SELECT 1 FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
      AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
      AND (pg_catalog.pg_has_role(c.relowner, 'MEMBER')
        OR (c.relkind <> 'S' AND pg_catalog.has_table_privilege(c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER'))
        OR (c.relkind = 'S' AND pg_catalog.has_sequence_privilege(c.oid, 'UPDATE')))
  ),
  pg_catalog.has_database_privilege(current_database(), 'CREATE') OR EXISTS (
    SELECT 1 FROM pg_catalog.pg_namespace n
    WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
      AND (pg_catalog.has_schema_privilege(n.oid, 'CREATE') OR pg_catalog.pg_has_role(n.nspowner, 'MEMBER'))
  ),
  ARRAY(
    SELECT DISTINCT n.nspname::text FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
      AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
      AND pg_catalog.has_schema_privilege(n.oid, 'USAGE')
      AND pg_catalog.has_table_privilege(c.oid, 'SELECT')
    ORDER BY 1
  )
FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`

// readDemoPostgresRoleFacts connects once with the mount's DSN and reads the facts
// the self-check judges. It is a variable so tests can substitute it.
var readDemoPostgresRoleFacts = func(ctx context.Context, dsn string) (demoPostgresRoleFacts, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return demoPostgresRoleFacts{}, errDemoPostgresUnreachable
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var facts demoPostgresRoleFacts
	if err := conn.QueryRow(ctx, demoPostgresRoleFactsSQL).Scan(&facts.Role, &facts.SessionRole, &facts.Privileged, &facts.PrivilegedMember, &facts.CanWrite, &facts.CanCreate, &facts.ReadableSchemas); err != nil {
		return demoPostgresRoleFacts{}, errDemoPostgresUnreachable
	}
	return facts, nil
}

// judgeDemoPostgresRole refuses a role that could write or become a role that
// could, a role named as an owner, a switched session, and a role that can read
// any schema other than the dataset's own one. The statement_timeout is no longer
// judged here: the library sets it on every mounted session.
func judgeDemoPostgresRole(facts demoPostgresRoleFacts, schemaName string) error {
	role := strings.ToLower(facts.Role)
	if role == "postgres" || role == "neondb_owner" || strings.HasSuffix(role, "_owner") || facts.Privileged {
		return errDemoPostgresOwnerRole
	}
	if facts.SessionRole != facts.Role {
		return errDemoPostgresRoleSwitched
	}
	if facts.PrivilegedMember {
		return errDemoPostgresPrivilegedGrant
	}
	if facts.CanWrite || facts.CanCreate {
		return errDemoPostgresWritableRole
	}
	if len(facts.ReadableSchemas) != 1 || facts.ReadableSchemas[0] != schemaName {
		return errDemoPostgresSchemaScope
	}
	return nil
}

// checkDemoPostgresSource is the pre-mount gate for one DemoDB source: DSN and
// TLS checks, then the live role self-check.
func checkDemoPostgresSource(ctx context.Context, dsn, schemaName string) error {
	if err := validateDemoPostgresDSN(dsn, demoPostgresTLSAllowedInsecurely()); err != nil {
		return err
	}
	facts, err := readDemoPostgresRoleFacts(ctx, dsn)
	if err != nil {
		return err
	}
	return judgeDemoPostgresRole(facts, schemaName)
}

// demoPostgresScopeError reports a mounted collection outside the dataset schema.
func demoPostgresScopeError(id, schemaName string) error {
	return fmt.Errorf("%w (database %s exposes a collection outside schema %s)", errDemoPostgresSchemaScope, id, schemaName)
}
