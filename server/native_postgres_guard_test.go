package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestDemoPostgresDSNRequiresVerifiedTLS(t *testing.T) {
	const ct = "&connect_timeout=10"
	for _, c := range []struct {
		name     string
		dsn      string
		insecure bool
		want     error
	}{
		{"URL verify-full", "postgres://reader:pw@ep-x.eu-central-1.aws.neon.tech/chinook?sslmode=verify-full" + ct, false, nil},
		{"URL verify-full with statement_timeout", "postgresql://reader:pw@ep-x.neon.tech:5432/chinook?sslmode=verify-full&statement_timeout=10000" + ct, false, nil},
		{"keyword verify-full", "host=ep-x.neon.tech user=reader password='p w' dbname=chinook sslmode=verify-full connect_timeout=10", false, nil},
		{"URL verify-ca", "postgresql://reader:pw@ep-x.neon.tech/chinook?sslmode=verify-ca" + ct, false, errDemoPostgresTLS},
		{"URL require", "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=require" + ct, false, errDemoPostgresTLS},
		{"URL prefer", "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=prefer" + ct, false, errDemoPostgresTLS},
		{"URL without sslmode", "postgres://reader:pw@ep-x.neon.tech/chinook?connect_timeout=10", false, errDemoPostgresTLS},
		{"keyword disable", "host=ep-x.neon.tech sslmode=disable connect_timeout=10", false, errDemoPostgresTLS},
		{"sslrootcert=system with require", "postgres://r@ep-x.neon.tech/db?sslmode=require&sslrootcert=system" + ct, false, errDemoPostgresTLS},
		// pgx keeps the first occurrence, a naive parser the last: both orders are refused.
		{"repeated sslmode, verify-full first", "postgres://r@h.example/db?sslmode=verify-full&sslmode=prefer" + ct, false, errDemoPostgresDSNAmbiguous},
		{"repeated sslmode, verify-full last", "postgres://r@h.example/db?sslmode=prefer&sslmode=verify-full" + ct, false, errDemoPostgresDSNAmbiguous},
		{"repeated keyword sslmode", "host=h.example sslmode=prefer sslmode=verify-full connect_timeout=10", false, errDemoPostgresDSNAmbiguous},
		{"repeated sslrootcert", "postgres://r@h.example/db?sslmode=verify-full&sslrootcert=a&sslrootcert=b" + ct, false, errDemoPostgresDSNAmbiguous},
		{"URL host and host parameter", "postgres://r@h.example/db?host=other.example&sslmode=verify-full" + ct, false, errDemoPostgresDSNAmbiguous},
		{"host and hostaddr", "host=h.example hostaddr=10.0.0.1 sslmode=verify-full connect_timeout=10", false, errDemoPostgresDSNAmbiguous},
		{"two hosts", "postgres://r@a.example,b.example/db?sslmode=verify-full" + ct, false, errDemoPostgresDSNAmbiguous},
		{"no host", "postgres:///db?sslmode=verify-full" + ct, false, errDemoPostgresDSNAmbiguous},
		{"unix socket host", "host=/tmp sslmode=verify-full connect_timeout=10", false, errDemoPostgresDSNAmbiguous},
		{"service file", "postgres://r@h.example/db?service=x&sslmode=verify-full" + ct, false, errDemoPostgresDSNAmbiguous},
		{"options parameter", "postgres://r@h.example/db?sslmode=verify-full&options=-c%20default_transaction_read_only%3Doff" + ct, false, errDemoPostgresOptionsParam},
		{"options overriding statement_timeout", "host=h.example sslmode=verify-full options='-c statement_timeout=0' connect_timeout=10", false, errDemoPostgresOptionsParam},
		{"search_path parameter", "postgres://r@h.example/db?sslmode=verify-full&search_path=public" + ct, false, errDemoPostgresRuntimeParam},
		{"read-only parameter off", "postgres://r@h.example/db?sslmode=verify-full&default_transaction_read_only=off" + ct, false, errDemoPostgresRuntimeParam},
		{"read-only parameter on", "postgres://r@h.example/db?sslmode=verify-full&default_transaction_read_only=on" + ct, false, nil},
		{"no connect_timeout", "postgres://r@h.example/db?sslmode=verify-full", false, errDemoPostgresConnectTimeout},
		{"connect_timeout too long", "postgres://r@h.example/db?sslmode=verify-full&connect_timeout=120", false, errDemoPostgresConnectTimeout},
		{"insecure flag on a remote host", "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=disable" + ct, true, errDemoPostgresInsecureHost},
		{"insecure flag on one remote host of two", "postgres://r@127.0.0.1:5432,ep-x.neon.tech:5432/db?sslmode=disable" + ct, true, errDemoPostgresDSNAmbiguous},
		{"insecure flag on loopback", "postgres://reader@127.0.0.1:55440/chinook?sslmode=disable" + ct, true, nil},
		{"insecure flag on IPv6 loopback", "postgres://reader@[::1]:55440/chinook?sslmode=disable" + ct, true, nil},
		{"insecure flag on localhost keywords", "host=localhost port=5432 sslmode=disable connect_timeout=10", true, nil},
		{"unparseable", "postgres://%zz", false, errDemoPostgresDSNInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := validateDemoPostgresDSNWithEnv(c.dsn, c.insecure, func(string) (string, bool) { return "", false })
			if !errors.Is(err, c.want) {
				t.Fatalf("validateDemoPostgresDSN = %v, want %v", err, c.want)
			}
		})
	}
}

func TestDemoPostgresDSNRefusesConnectionEnvironment(t *testing.T) {
	const dsn = "postgres://r@h.example/db?sslmode=verify-full&connect_timeout=10"
	for _, name := range []string{"PGSSLMODE", "PGHOST", "PGSERVICE", "PGOPTIONS", "PGSSLROOTCERT"} {
		env := func(key string) (string, bool) {
			if key == name {
				return "x", true
			}
			return "", false
		}
		if err := validateDemoPostgresDSNWithEnv(dsn, false, env); !errors.Is(err, errDemoPostgresEnv) {
			t.Errorf("%s set: err=%v, want environment refusal", name, err)
		}
	}
	t.Setenv("PGSSLMODE", "disable")
	if err := validateDemoPostgresDSN(dsn, false); !errors.Is(err, errDemoPostgresEnv) {
		t.Errorf("process PGSSLMODE: err=%v", err)
	}
}

func TestDemoPostgresInsecureTLSFlagIsExplicit(t *testing.T) {
	for value, want := range map[string]bool{"": false, "0": false, "false": false, "yes": false, "1": true, "true": true} {
		t.Setenv(demoPostgresInsecureTLSEnv, value)
		if got := demoPostgresTLSAllowedInsecurely(); got != want {
			t.Errorf("%s=%q allows insecure TLS = %v, want %v", demoPostgresInsecureTLSEnv, value, got, want)
		}
	}
}

func TestDemoPostgresNeonPoolerIsDetected(t *testing.T) {
	if !demoPostgresNeonPooler("postgres://r:p@ep-cool-1-pooler.eu-central-1.aws.neon.tech/db?sslmode=verify-full") {
		t.Error("pooled endpoint not detected")
	}
	if demoPostgresNeonPooler("postgres://r:p@ep-cool-1.eu-central-1.aws.neon.tech/db?sslmode=verify-full") {
		t.Error("direct endpoint reported as pooled")
	}
}

func TestDemoPostgresRoleSelfCheck(t *testing.T) {
	safe := demoPostgresRoleFacts{Role: "chinook_reader", SessionRole: "chinook_reader", StatementTimeoutMS: 10_000, ReadableSchemas: []string{"chinook"}}
	for _, c := range []struct {
		name    string
		mutate  func(*demoPostgresRoleFacts)
		library bool
		want    error
	}{
		{"dedicated reader", func(*demoPostgresRoleFacts) {}, false, nil},
		{"neon owner role", func(f *demoPostgresRoleFacts) { f.Role = "neondb_owner" }, false, errDemoPostgresOwnerRole},
		{"any owner-named role", func(f *demoPostgresRoleFacts) { f.Role = "chinook_owner" }, false, errDemoPostgresOwnerRole},
		{"postgres", func(f *demoPostgresRoleFacts) { f.Role = "postgres" }, false, errDemoPostgresOwnerRole},
		{"privileged attributes", func(f *demoPostgresRoleFacts) { f.Privileged = true }, false, errDemoPostgresOwnerRole},
		{"member of a privileged role (even without INHERIT)", func(f *demoPostgresRoleFacts) { f.PrivilegedMember = true }, false, errDemoPostgresPrivilegedGrant},
		{"switched session", func(f *demoPostgresRoleFacts) { f.SessionRole = "someone_else" }, false, errDemoPostgresRoleSwitched},
		{"table write grant", func(f *demoPostgresRoleFacts) { f.CanWrite = true }, false, errDemoPostgresWritableRole},
		{"create grant", func(f *demoPostgresRoleFacts) { f.CanCreate = true }, false, errDemoPostgresWritableRole},
		{"no statement timeout", func(f *demoPostgresRoleFacts) { f.StatementTimeoutMS = 0 }, false, errDemoPostgresStatementTime},
		{"statement timeout too long", func(f *demoPostgresRoleFacts) { f.StatementTimeoutMS = 120_000 }, false, errDemoPostgresStatementTime},
		{"no statement timeout once the library bounds statements", func(f *demoPostgresRoleFacts) { f.StatementTimeoutMS = 0 }, true, nil},
		{"second readable schema", func(f *demoPostgresRoleFacts) { f.ReadableSchemas = []string{"chinook", "northwind"} }, false, errDemoPostgresSchemaScope},
		{"other schema only", func(f *demoPostgresRoleFacts) { f.ReadableSchemas = []string{"northwind"} }, false, errDemoPostgresSchemaScope},
		{"nothing readable", func(f *demoPostgresRoleFacts) { f.ReadableSchemas = nil }, false, errDemoPostgresSchemaScope},
	} {
		t.Run(c.name, func(t *testing.T) {
			facts := safe
			facts.ReadableSchemas = append([]string(nil), safe.ReadableSchemas...)
			c.mutate(&facts)
			if err := judgeDemoPostgresRole(facts, "chinook", c.library); !errors.Is(err, c.want) {
				t.Fatalf("judgeDemoPostgresRole = %v, want %v", err, c.want)
			}
		})
	}
}

func TestDemoPostgresSourceCheckRunsTLSBeforeConnecting(t *testing.T) {
	connected := false
	original := readDemoPostgresRoleFacts
	t.Cleanup(func() { readDemoPostgresRoleFacts = original })
	readDemoPostgresRoleFacts = func(context.Context, string) (demoPostgresRoleFacts, error) {
		connected = true
		return demoPostgresRoleFacts{Role: "chinook_reader", SessionRole: "chinook_reader", StatementTimeoutMS: 5000, ReadableSchemas: []string{"chinook"}}, nil
	}
	t.Setenv(demoPostgresInsecureTLSEnv, "")
	if err := checkDemoPostgresSource(context.Background(), "postgres://r@ep-x.neon.tech/chinook?sslmode=require&connect_timeout=10", "chinook"); !errors.Is(err, errDemoPostgresTLS) || connected {
		t.Fatalf("unverified TLS: err=%v connected=%v", err, connected)
	}
	if err := checkDemoPostgresSource(context.Background(), "postgres://r@ep-x.neon.tech/chinook?sslmode=verify-full&connect_timeout=10", "chinook"); err != nil || !connected {
		t.Fatalf("verified TLS: err=%v connected=%v", err, connected)
	}
	if err := checkDemoPostgresSource(context.Background(), "postgres://r@ep-x.neon.tech/northwind?sslmode=verify-full&connect_timeout=10", "northwind"); !errors.Is(err, errDemoPostgresSchemaScope) {
		t.Fatalf("scope mismatch: err=%v", err)
	}
}

func TestDemoPostgresMountedScopeRejectsForeignCollections(t *testing.T) {
	provider := runtimeDatabase{ID: "chinook-postgresql", nativePostgres: &nativePostgresMountConfig{schemaName: "chinook", dsnEnv: "OVDB_PG_CHINOOK_DSN"}}
	own, err := schema.NativePostgresCollectionID("chinook", "Album")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := schema.NativePostgresCollectionID("public", "secrets")
	if err != nil {
		t.Fatal(err)
	}
	database := func(ids ...string) *core.Database {
		collections := map[string]schema.Collection{}
		for _, id := range ids {
			collections[id] = schema.Collection{}
		}
		return &core.Database{Manifest: &manifest.Manifest{Schemas: &schema.Schemas{Collections: collections}}}
	}
	if err := checkDemoPostgresMountedScope(database(own), provider); err != nil {
		t.Fatalf("own schema refused: %v", err)
	}
	for _, ids := range [][]string{{own, foreign}, {"Album"}} {
		if err := checkDemoPostgresMountedScope(database(ids...), provider); !errors.Is(err, errDemoPostgresSchemaScope) {
			t.Errorf("collections %q: err=%v, want scope refusal", ids, err)
		}
	}
}

func TestDemoPostgresErrorsAreRedacted(t *testing.T) {
	dsn := "postgres://reader_x:hunter2-secret@ep-x.neon.tech:6543/chinook_db?sslmode=verify-full&connect_timeout=10"
	message := redactDemoPostgresText("failed to connect to `user=reader_x database=chinook_db`: ep-x.neon.tech:6543 (ep-x.neon.tech): dial "+dsn+" failed for hunter2-secret", dsn)
	for _, secret := range []string{"hunter2-secret", "ep-x.neon.tech", "6543", "reader_x", "chinook_db"} {
		if strings.Contains(message, secret) {
			t.Fatalf("redacted message still discloses %q: %s", secret, message)
		}
	}
	keyword := "host=h.example user=r password=topsecretvalue sslmode=verify-full"
	if message := redactDemoPostgresText("bad password topsecretvalue", keyword); strings.Contains(message, "topsecretvalue") {
		t.Fatalf("keyword password disclosed: %s", message)
	}
}

func TestDemoPostgresReasonCodesAreStable(t *testing.T) {
	for err, want := range map[error]string{
		errDemoPostgresTLS: "tls_required",
		fmt.Errorf("wrap: %w", errDemoPostgresDSNAmbiguous):         "dsn_rejected",
		errDemoPostgresPrivilegedGrant:                              "role_unsafe",
		errDemoPostgresUnreachable:                                  "connection_failed",
		fmt.Errorf("%w (90s)", errDemoPostgresTimeout):              "timeout",
		fmt.Errorf("%w: dial 10.1.2.3", errDemoPostgresMountFailed): "mount_failed",
		errors.New("unclassified 10.1.2.3:5432"):                    "mount_failed",
		demoPostgresScopeError("chinook-postgresql", "chinook"):     "schema_scope",
	} {
		if got := demoPostgresReasonCode(err); got != want {
			t.Errorf("reason code of %v = %q, want %q", err, got, want)
		}
	}
}

func TestLibraryMountHardeningSeamIsInert(t *testing.T) {
	// Until the openvaultdb-go release with per-mount hardening is pinned, the
	// seam must not change the options and the in-repo checks must stay on.
	options := mount.Options{CatalogueDir: "kept"}
	applyLibraryMountHardening(&options)
	if options.CatalogueDir != "kept" || libraryEnforcesMountHardening {
		t.Fatalf("library hardening seam changed options or claims enforcement: %+v", options)
	}
}

// TestDemoPostgresRoleSelfCheckAgainstRealPostgres runs the live self-check:
// the reader role behind OVDB_PG_CHINOOK_DSN must pass, and a privileged role
// (OVDB_PG_TEST_PRIVILEGED_DSN, e.g. the local superuser) must be refused.
func TestDemoPostgresRoleSelfCheckAgainstRealPostgres(t *testing.T) {
	reader := os.Getenv("OVDB_PG_CHINOOK_DSN")
	if reader == "" {
		t.Skip("set OVDB_PG_CHINOOK_DSN to a read-only role on a loaded chinook schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := checkDemoPostgresSource(ctx, reader, "chinook"); err != nil {
		t.Fatalf("reader role refused: %v", err)
	}
	if err := checkDemoPostgresSource(ctx, reader, "northwind"); !errors.Is(err, errDemoPostgresSchemaScope) {
		t.Fatalf("reader role for another dataset: %v, want scope refusal", err)
	}
	if privileged := os.Getenv("OVDB_PG_TEST_PRIVILEGED_DSN"); privileged != "" {
		if err := checkDemoPostgresSource(ctx, privileged, "chinook"); !errors.Is(err, errDemoPostgresOwnerRole) {
			t.Fatalf("privileged role: %v, want owner refusal", err)
		}
	}
}

// TestDemoPostgresRoleSelfCheckRefusesPrivilegedMemberships reproduces, on a real
// PostgreSQL, roles that pass an attribute-only check but can still become or act
// as a privileged role. It needs OVDB_PG_CHINOOK_DSN (URL form) and a superuser
// OVDB_PG_TEST_PRIVILEGED_DSN on the same database; it creates and drops its
// own throwaway roles.
func TestDemoPostgresRoleSelfCheckRefusesPrivilegedMemberships(t *testing.T) {
	reader, admin := os.Getenv("OVDB_PG_CHINOOK_DSN"), os.Getenv("OVDB_PG_TEST_PRIVILEGED_DSN")
	if reader == "" || admin == "" {
		t.Skip("set OVDB_PG_CHINOOK_DSN and a superuser OVDB_PG_TEST_PRIVILEGED_DSN on the same chinook database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	roleDSN := func(role string) string {
		parsed, err := url.Parse(reader)
		if err != nil {
			t.Fatal(err)
		}
		parsed.User = url.User(role)
		return parsed.String()
	}
	newReader := func(role string) string {
		t.Helper()
		t.Cleanup(func() {
			_, _ = conn.Exec(context.Background(), "DROP OWNED BY "+role)
			_, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
		})
		exec("DROP ROLE IF EXISTS " + role)
		exec("CREATE ROLE " + role + " LOGIN")
		exec("GRANT USAGE ON SCHEMA chinook TO " + role)
		exec("GRANT SELECT ON ALL TABLES IN SCHEMA chinook TO " + role)
		exec("ALTER ROLE " + role + " SET statement_timeout = '10s'")
		return roleDSN(role)
	}

	if err := checkDemoPostgresSource(ctx, newReader("ovdb_it_plain"), "chinook"); err != nil {
		t.Fatalf("a plain reader is refused: %v", err)
	}

	t.Run("superuser granted WITH INHERIT FALSE, SET TRUE", func(t *testing.T) {
		dsn := newReader("ovdb_it_setrole")
		exec("GRANT postgres TO ovdb_it_setrole WITH INHERIT FALSE, SET TRUE")
		// The reviewer's scenario: the role can become the superuser and write.
		escalated, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = escalated.Close(context.Background()) }()
		if _, err := escalated.Exec(ctx, "SET ROLE postgres; CREATE TABLE chinook.ovdb_it_escalated(x int); DROP TABLE chinook.ovdb_it_escalated; RESET ROLE"); err != nil {
			t.Fatalf("scenario is not reproducible: %v", err)
		}
		if err := checkDemoPostgresSource(ctx, dsn, "chinook"); !errors.Is(err, errDemoPostgresPrivilegedGrant) {
			t.Fatalf("err = %v, want privileged membership refusal", err)
		}
	})
	for _, builtin := range []string{"pg_execute_server_program", "pg_read_server_files", "pg_write_server_files", "pg_write_all_data"} {
		t.Run(builtin, func(t *testing.T) {
			role := "ovdb_it_" + strings.TrimPrefix(builtin, "pg_")
			dsn := newReader(role)
			exec("GRANT " + builtin + " TO " + role)
			if err := checkDemoPostgresSource(ctx, dsn, "chinook"); demoPostgresReasonCode(err) != "role_unsafe" {
				t.Fatalf("err = %v, want a role_unsafe refusal", err)
			}
		})
	}
	t.Run("member of a role that owns objects, without INHERIT", func(t *testing.T) {
		dsn := newReader("ovdb_it_ownermember")
		t.Cleanup(func() {
			_, _ = conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS ovdb_it_scratch CASCADE")
			_, _ = conn.Exec(context.Background(), "DROP OWNED BY ovdb_it_owner")
			_, _ = conn.Exec(context.Background(), "DROP ROLE IF EXISTS ovdb_it_owner")
		})
		exec("DROP SCHEMA IF EXISTS ovdb_it_scratch CASCADE")
		exec("DROP ROLE IF EXISTS ovdb_it_owner")
		exec("CREATE ROLE ovdb_it_owner NOLOGIN")
		exec("CREATE SCHEMA ovdb_it_scratch AUTHORIZATION ovdb_it_owner")
		exec("CREATE TABLE ovdb_it_scratch.t (x int)")
		exec("ALTER TABLE ovdb_it_scratch.t OWNER TO ovdb_it_owner")
		exec("GRANT ovdb_it_owner TO ovdb_it_ownermember WITH INHERIT FALSE, SET TRUE")
		if err := checkDemoPostgresSource(ctx, dsn, "chinook"); !errors.Is(err, errDemoPostgresPrivilegedGrant) {
			t.Fatalf("err = %v, want privileged membership refusal", err)
		}
	})
}
