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
// sslmode=disable. It is honoured only when every host in the DSN is a loopback
// address, so it cannot weaken a Neon (or any other remote) connection, and it is
// never set by the deploy workflow.
const demoPostgresInsecureTLSEnv = "OVDB_PG_INSECURE_TLS_FOR_TESTS"

// demoPostgresMaxStatementTimeout bounds the session statement_timeout a demo
// role must carry (as ALTER ROLE ... SET statement_timeout, or a
// statement_timeout parameter in the DSN). Zero (unbounded) is refused.
const demoPostgresMaxStatementTimeout = 60 * time.Second

// demoPostgresStatementTimeout is the per-statement bound the library will be
// asked to apply once it supports mount.Options.StatementTimeout.
const demoPostgresStatementTimeout = 10 * time.Second

// libraryEnforcesMountHardening is the one switch of the go.mod bump to the
// openvaultdb-go release that adds per-mount hardening options. Until then the
// mount runs with the v0.24.1 options and this repository performs the checks
// itself (TLS mode, role privileges, statement_timeout, schema scope, and
// per-mount isolation in demoPostgresMounter).
//
// Follow-up when the release lands: set this to true, uncomment the assignments
// in applyLibraryMountHardening, adapt names if the released API differs.
const libraryEnforcesMountHardening = false

// applyLibraryMountHardening is the seam for the library's per-mount options.
// The intended names, from the sibling openvaultdb-go change, are:
//
//	options.Required = false                                   // skip a failing mount, do not fail the server
//	options.RequireSafeRole = true                             // refuse superuser/owner/writable roles
//	options.StatementTimeout = demoPostgresStatementTimeout    // SET statement_timeout per session
//	options.TLSVerify = true                                   // require verify-full/verify-ca
//
// The in-repo checks below stay in place after the bump: they are cheap and
// keep the refusal reasons this service reports.
func applyLibraryMountHardening(options *mount.Options) {
	_ = options
}

// errDemoPostgresTLS and its siblings classify a refused mount. Their messages
// are fixed strings: they are logged and served by the status route, so they
// must never carry a DSN, host, user or password.
var (
	errDemoPostgresDSNInvalid    = errors.New("DSN is not a valid PostgreSQL connection string")
	errDemoPostgresTLS           = errors.New("DSN must set sslmode=verify-full or sslmode=verify-ca")
	errDemoPostgresInsecureHost  = errors.New(demoPostgresInsecureTLSEnv + " is honoured only for loopback hosts")
	errDemoPostgresOptionsParam  = errors.New("DSN must not set the PostgreSQL options parameter")
	errDemoPostgresOwnerRole     = errors.New("role is an owner or privileged role; use a dedicated read-only role")
	errDemoPostgresWritableRole  = errors.New("role can write, create or own objects; grant SELECT and USAGE only")
	errDemoPostgresStatementTime = errors.New("role has no statement_timeout between 1ms and 60s")
	errDemoPostgresSchemaScope   = errors.New("role can read relations outside its own schema")
	errDemoPostgresUnreachable   = errors.New("could not connect to PostgreSQL or run the role self-check")
)

// demoPostgresTLSAllowedInsecurely reports whether the test-only TLS bypass is set.
func demoPostgresTLSAllowedInsecurely() bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(demoPostgresInsecureTLSEnv)))
	return err == nil && value
}

// validateDemoPostgresDSN refuses a DSN that would connect without verifying the
// server certificate. The connection string is parsed, never logged.
func validateDemoPostgresDSN(dsn string, allowInsecureLoopback bool) error {
	params, hosts, err := demoPostgresDSNParams(dsn)
	if err != nil {
		return errDemoPostgresDSNInvalid
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return errDemoPostgresDSNInvalid
	}
	if strings.TrimSpace(params["options"]) != "" {
		return errDemoPostgresOptionsParam
	}
	switch params["sslmode"] {
	case "verify-full", "verify-ca":
		return nil
	}
	if !allowInsecureLoopback {
		return errDemoPostgresTLS
	}
	if len(hosts) == 0 {
		return errDemoPostgresInsecureHost
	}
	for _, host := range hosts {
		if !loopbackHost(host) {
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

// demoPostgresNeonPooler reports whether any host is a Neon pooled endpoint
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
// URL or keyword/value DSN. Environment fallbacks (PGSSLMODE, PGHOST) are
// deliberately not consulted: the verification mode must be written in the DSN.
func demoPostgresDSNParams(dsn string) (map[string]string, []string, error) {
	params := map[string]string{}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return nil, nil, err
		}
		for key, values := range parsed.Query() {
			if len(values) > 0 {
				params[key] = values[len(values)-1]
			}
		}
		var hosts []string
		for _, hostPort := range strings.Split(parsed.Host, ",") {
			if hostPort == "" {
				continue
			}
			host := hostPort
			if h, _, err := net.SplitHostPort(hostPort); err == nil {
				host = h
			}
			hosts = append(hosts, host)
		}
		if value := params["host"]; value != "" {
			hosts = append(hosts, strings.Split(value, ",")...)
		}
		return params, hosts, nil
	}
	for rest := strings.TrimSpace(dsn); rest != ""; rest = strings.TrimSpace(rest) {
		key, value, ok := strings.Cut(rest, "=")
		if !ok {
			return nil, nil, errors.New("keyword without value")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimLeft(value, " \t")
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
			params[key] = b.String()
			rest = value[i+1:]
		} else {
			end := strings.IndexAny(value, " \t")
			if end < 0 {
				end = len(value)
			}
			params[key] = value[:end]
			rest = value[end:]
		}
	}
	var hosts []string
	if value := params["host"]; value != "" {
		hosts = strings.Split(value, ",")
	}
	if value := params["hostaddr"]; value != "" {
		hosts = append(hosts, strings.Split(value, ",")...)
	}
	return params, hosts, nil
}

// demoPostgresRoleFacts is what the role self-check reads from PostgreSQL.
type demoPostgresRoleFacts struct {
	Role               string
	Privileged         bool // superuser, createrole, createdb, replication or bypassrls
	CanWrite           bool // owns, or may write/alter, any non-system relation or sequence
	CanCreate          bool // may create in any non-system schema, or create schemas in the database
	StatementTimeoutMS int64
	ReadableSchemas    []string // non-system schemas holding at least one SELECT-able relation
}

const demoPostgresRoleFactsSQL = `
SELECT
  current_user,
  (r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls),
  EXISTS (
    SELECT 1 FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
      AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
      AND (pg_catalog.pg_has_role(c.relowner, 'USAGE')
        OR (c.relkind <> 'S' AND pg_catalog.has_table_privilege(c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER'))
        OR (c.relkind = 'S' AND pg_catalog.has_sequence_privilege(c.oid, 'UPDATE')))
  ),
  pg_catalog.has_database_privilege(current_database(), 'CREATE') OR EXISTS (
    SELECT 1 FROM pg_catalog.pg_namespace n
    WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
      AND (pg_catalog.has_schema_privilege(n.oid, 'CREATE') OR pg_catalog.pg_has_role(n.nspowner, 'USAGE'))
  ),
  (SELECT setting::bigint FROM pg_catalog.pg_settings WHERE name = 'statement_timeout'),
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
	if err := conn.QueryRow(ctx, demoPostgresRoleFactsSQL).Scan(&facts.Role, &facts.Privileged, &facts.CanWrite, &facts.CanCreate, &facts.StatementTimeoutMS, &facts.ReadableSchemas); err != nil {
		return demoPostgresRoleFacts{}, errDemoPostgresUnreachable
	}
	return facts, nil
}

// judgeDemoPostgresRole refuses a role that could write, a role named as an owner,
// a session without a bounded statement_timeout, and a role that can read any
// schema other than the dataset's own one.
func judgeDemoPostgresRole(facts demoPostgresRoleFacts, schemaName string, libraryBoundsStatements bool) error {
	role := strings.ToLower(facts.Role)
	if role == "postgres" || role == "neondb_owner" || strings.HasSuffix(role, "_owner") || facts.Privileged {
		return errDemoPostgresOwnerRole
	}
	if facts.CanWrite || facts.CanCreate {
		return errDemoPostgresWritableRole
	}
	if !libraryBoundsStatements && (facts.StatementTimeoutMS <= 0 || facts.StatementTimeoutMS > demoPostgresMaxStatementTimeout.Milliseconds()) {
		return errDemoPostgresStatementTime
	}
	if len(facts.ReadableSchemas) != 1 || facts.ReadableSchemas[0] != schemaName {
		return errDemoPostgresSchemaScope
	}
	return nil
}

// checkDemoPostgresSource is the pre-mount gate for one DemoDB source: TLS mode,
// then the live role self-check.
func checkDemoPostgresSource(ctx context.Context, dsn, schemaName string) error {
	if err := validateDemoPostgresDSN(dsn, demoPostgresTLSAllowedInsecurely()); err != nil {
		return err
	}
	facts, err := readDemoPostgresRoleFacts(ctx, dsn)
	if err != nil {
		return err
	}
	return judgeDemoPostgresRole(facts, schemaName, libraryEnforcesMountHardening)
}

// demoPostgresScopeError reports a mounted collection outside the dataset schema.
func demoPostgresScopeError(id, schemaName string) error {
	return fmt.Errorf("%w (database %s exposes a collection outside schema %s)", errDemoPostgresSchemaScope, id, schemaName)
}
