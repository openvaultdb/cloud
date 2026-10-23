package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

func TestDemoPostgresPoolerHostsAreRefused(t *testing.T) {
	for host, want := range map[string]bool{
		"ep-cool-1-pooler.eu-central-1.aws.neon.tech": true,
		"EP-COOL-1-POOLER.eu-central-1.aws.neon.tech": true,
		"ep-cool-1-pooler":                       true,
		"db-pooler.internal.example.com":         true,
		"my-pooler.example.com":                  true,
		"pgbouncer.internal":                     true,
		"db.PgBouncer.example.com":               true,
		"ep-cool-1.eu-central-1.aws.neon.tech":   false,
		"ep-pooler-1.eu-central-1.aws.neon.tech": false,
		"127.0.0.1":                              false,
		"localhost":                              false,
	} {
		if got := demoPostgresPoolerHost(host); got != want {
			t.Errorf("demoPostgresPoolerHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestValidateDemoPostgresDSNRefusesPoolerEndpoints(t *testing.T) {
	const tail = "sslmode=verify-full&connect_timeout=10"
	for _, dsn := range []string{
		"postgres://r:p@ep-cool-1-pooler.eu-central-1.aws.neon.tech/db?" + tail,
		"postgresql://r:p@ep-cool-1-pooler.eu-central-1.aws.neon.tech:5432/db?" + tail,
		"postgres://r:p@db-pooler.example.com/db?" + tail,
		"postgres://r:p@pgbouncer.example.com/db?" + tail,
		"postgres://r:p@db.example.com/db?host=ep-cool-1-pooler.eu-central-1.aws.neon.tech&" + tail, // ambiguous host: still refused
		"host=ep-cool-1-pooler.eu-central-1.aws.neon.tech user=r dbname=db sslmode=verify-full connect_timeout=10",
	} {
		err := validateDemoPostgresDSNWithEnv(dsn, false, func(string) (string, bool) { return "", false })
		if err == nil {
			t.Errorf("pooler DSN accepted: %s", dsn)
			continue
		}
		if code := demoPostgresReasonCode(err); code != "pooler_endpoint" && code != "dsn_rejected" {
			t.Errorf("pooler DSN %s: reason %q (%v)", dsn, code, err)
		}
		if strings.Contains(err.Error(), "pooler.eu-central-1") {
			t.Errorf("error discloses the host: %v", err)
		}
	}
	direct := "postgres://r:p@ep-cool-1.eu-central-1.aws.neon.tech/db?" + tail
	if err := validateDemoPostgresDSNWithEnv(direct, false, func(string) (string, bool) { return "", false }); err != nil {
		t.Errorf("direct endpoint refused: %v", err)
	}
	err := validateDemoPostgresDSNWithEnv("postgres://r:p@ep-cool-1-pooler.eu-central-1.aws.neon.tech/db?"+tail, false, func(string) (string, bool) { return "", false })
	if !errors.Is(err, errDemoPostgresPoolerEndpoint) || !strings.Contains(err.Error(), "direct endpoint") {
		t.Errorf("pooler error = %v, want errDemoPostgresPoolerEndpoint naming the direct endpoint", err)
	}
}

func TestDemoPostgresLibraryRoleRefusalMapsToRoleUnsafe(t *testing.T) {
	refusal := errors.New(demoPostgresLibraryRoleRefusalText + ": may execute SECURITY DEFINER function chinook.f(); connect as a role with only SELECT on the exposed relations (and USAGE on their schemas)")
	for _, err := range []error{refusal, fmt.Errorf("%w: %w", errDemoPostgresMountFailed, refusal)} {
		if got := demoPostgresReasonCode(err); got != "role_unsafe" {
			t.Errorf("reason of %v = %q, want role_unsafe", err, got)
		}
	}
	if got := demoPostgresReasonCode(fmt.Errorf("%w: could not list PostgreSQL schemas", errDemoPostgresMountFailed)); got != "mount_failed" {
		t.Errorf("unrelated mount failure = %q, want mount_failed", got)
	}
}

// TestDemoPostgresLibraryRoleRefusalTextMatchesLibrary fails when the pinned
// openvaultdb-go changes the text of its role refusal, which the library offers
// no sentinel or error type for. The real-PostgreSQL test below also checks the
// reason code end to end.
func TestDemoPostgresLibraryRoleRefusalTextMatchesLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/openvaultdb/openvaultdb-go").Output()
	if err != nil {
		t.Skipf("cannot locate the openvaultdb-go module: %v", err)
	}
	source, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "pkg", "mount", "postgres.go"))
	if err != nil {
		t.Skipf("cannot read the library source: %v", err)
	}
	if !strings.Contains(string(source), demoPostgresLibraryRoleRefusalText+": %s") {
		t.Fatalf("openvaultdb-go no longer returns %q: update demoPostgresLibraryRoleRefusalText and the role_unsafe mapping", demoPostgresLibraryRoleRefusalText)
	}
}

func TestDemoPostgresRoleSelfCheck(t *testing.T) {
	safe := demoPostgresRoleFacts{Role: "chinook_reader", SessionRole: "chinook_reader", ReadableSchemas: []string{"chinook"}}
	for _, c := range []struct {
		name   string
		mutate func(*demoPostgresRoleFacts)
		want   error
	}{
		{"dedicated reader", func(*demoPostgresRoleFacts) {}, nil},
		{"neon owner role", func(f *demoPostgresRoleFacts) { f.Role = "neondb_owner" }, errDemoPostgresOwnerRole},
		{"any owner-named role", func(f *demoPostgresRoleFacts) { f.Role = "chinook_owner" }, errDemoPostgresOwnerRole},
		{"postgres", func(f *demoPostgresRoleFacts) { f.Role = "postgres" }, errDemoPostgresOwnerRole},
		{"privileged attributes", func(f *demoPostgresRoleFacts) { f.Privileged = true }, errDemoPostgresOwnerRole},
		{"member of a privileged role (even without INHERIT)", func(f *demoPostgresRoleFacts) { f.PrivilegedMember = true }, errDemoPostgresPrivilegedGrant},
		{"switched session", func(f *demoPostgresRoleFacts) { f.SessionRole = "someone_else" }, errDemoPostgresRoleSwitched},
		{"table write grant", func(f *demoPostgresRoleFacts) { f.CanWrite = true }, errDemoPostgresWritableRole},
		{"create grant", func(f *demoPostgresRoleFacts) { f.CanCreate = true }, errDemoPostgresWritableRole},
		{"second readable schema", func(f *demoPostgresRoleFacts) { f.ReadableSchemas = []string{"chinook", "northwind"} }, errDemoPostgresSchemaScope},
		{"other schema only", func(f *demoPostgresRoleFacts) { f.ReadableSchemas = []string{"northwind"} }, errDemoPostgresSchemaScope},
		{"nothing readable", func(f *demoPostgresRoleFacts) { f.ReadableSchemas = nil }, errDemoPostgresSchemaScope},
	} {
		t.Run(c.name, func(t *testing.T) {
			facts := safe
			facts.ReadableSchemas = append([]string(nil), safe.ReadableSchemas...)
			c.mutate(&facts)
			if err := judgeDemoPostgresRole(facts, "chinook"); !errors.Is(err, c.want) {
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
		return demoPostgresRoleFacts{Role: "chinook_reader", SessionRole: "chinook_reader", ReadableSchemas: []string{"chinook"}}, nil
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
		errDemoPostgresTLS:                                          "tls_required",
		errDemoPostgresPoolerEndpoint:                               "pooler_endpoint",
		fmt.Errorf("wrap: %w", errDemoPostgresDSNAmbiguous):         "dsn_rejected",
		errDemoPostgresPrivilegedGrant:                              "role_unsafe",
		errDemoPostgresUnreachable:                                  "connection_failed",
		fmt.Errorf("%w (90s)", errDemoPostgresTimeout):              "timeout",
		fmt.Errorf("%w: dial 10.1.2.3", errDemoPostgresMountFailed): "mount_failed",
		// The library's catalogue deadline (openvaultdb-go wraps context.DeadlineExceeded).
		fmt.Errorf("%w: m.yaml: discovery did not finish in time (60s, %w): could not describe", errDemoPostgresMountFailed, context.DeadlineExceeded): "timeout",
		// A finished role refusal stays a refusal even when the deadline also passed.
		fmt.Errorf("%w: %s: x (%w)", errDemoPostgresMountFailed, demoPostgresLibraryRoleRefusalText, context.DeadlineExceeded): "role_unsafe",
		errors.New("unclassified 10.1.2.3:5432"):                "mount_failed",
		demoPostgresScopeError("chinook-postgresql", "chinook"): "schema_scope",
	} {
		if got := demoPostgresReasonCode(err); got != want {
			t.Errorf("reason code of %v = %q, want %q", err, got, want)
		}
	}
}

func TestLibraryMountHardeningOptionsAreSet(t *testing.T) {
	options := mount.Options{CatalogueDir: "kept"}
	applyLibraryMountHardening(&options)
	if options.CatalogueDir != "kept" || !options.RequireReadOnlyNativePostgresRole || options.NativePostgresStatementTimeout != 15*time.Second || options.NativePostgresDiscoveryTimeout != 60*time.Second || options.AllowInsecureNativePostgresTransport {
		t.Fatalf("library hardening options = %+v", options)
	}
	options.AllowInsecureNativePostgresTransport = true
	applyLibraryMountHardening(&options)
	if options.AllowInsecureNativePostgresTransport {
		t.Fatal("the seam must force verified TLS for remote hosts")
	}
	mounted, err := demoPostgresMountOptions(runtimeDatabase{ID: "chinook-postgresql", nativePostgres: &nativePostgresMountConfig{schemaName: "chinook", dsnEnv: "OVDB_PG_CHINOOK_DSN"}})
	if err != nil || !mounted.RequireReadOnlyNativePostgresRole || mounted.NativePostgresStatementTimeout != demoPostgresStatementTimeout {
		t.Fatalf("mount options do not carry the hardening: %+v, %v", mounted, err)
	}
}

func TestDemoPostgresDSNRefusesLibraryInsecureTransportEnv(t *testing.T) {
	const dsn = "postgres://r@h.example/db?sslmode=verify-full&connect_timeout=10"
	for _, value := range []string{"true", "false", "1"} {
		env := func(key string) (string, bool) {
			if key == mount.InsecureNativePostgresTransportEnv {
				return value, true
			}
			return "", false
		}
		err := validateDemoPostgresDSNWithEnv(dsn, false, env)
		if !errors.Is(err, errDemoPostgresInsecureEnv) || demoPostgresReasonCode(err) != "tls_required" {
			t.Errorf("%s=%s: err=%v, want tls_required refusal", mount.InsecureNativePostgresTransportEnv, value, err)
		}
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

// TestDemoPostgresLibraryHardeningAgainstRealPostgres proves, on a real
// PostgreSQL, what the library now enforces for this service: a reader role with
// no statement_timeout of its own is mounted and its sessions run with the
// library's 15s statement_timeout inside a read-only transaction; and a role the
// library considers writable (here: EXECUTE on a SECURITY DEFINER function in the
// dataset schema, which this service's own self-check does not look at) is refused
// by the mount itself. It needs OVDB_PG_CHINOOK_DSN (URL form) and a superuser
// OVDB_PG_TEST_PRIVILEGED_DSN on the same database; it creates and drops its own
// throwaway roles, view and function.
func TestDemoPostgresLibraryHardeningAgainstRealPostgres(t *testing.T) {
	reader, admin := os.Getenv("OVDB_PG_CHINOOK_DSN"), os.Getenv("OVDB_PG_TEST_PRIVILEGED_DSN")
	if reader == "" || admin == "" {
		t.Skip("set OVDB_PG_CHINOOK_DSN and a superuser OVDB_PG_TEST_PRIVILEGED_DSN on the same chinook database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
		parsed, err := url.Parse(reader)
		if err != nil {
			t.Fatal(err)
		}
		parsed.User = url.User(role)
		query := parsed.Query()
		query.Del("statement_timeout")
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	provider := runtimeDatabase{ID: "chinook-postgresql", CORSOrigins: demoPostgresCORSOrigins("chinook"), nativePostgres: &nativePostgresMountConfig{schemaName: "chinook", dsnEnv: "OVDB_PG_CHINOOK_DSN"}}

	t.Run("sessions are bounded and read-only without a role timeout", func(t *testing.T) {
		t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP VIEW IF EXISTS chinook.ovdb_it_session") })
		exec("DROP VIEW IF EXISTS chinook.ovdb_it_session")
		exec("CREATE VIEW chinook.ovdb_it_session AS SELECT current_setting('statement_timeout') AS statement_timeout, current_setting('transaction_read_only') AS read_only")
		dsn := newReader("ovdb_it_notimeout")
		exec("GRANT SELECT ON chinook.ovdb_it_session TO ovdb_it_notimeout")
		if err := checkDemoPostgresSource(ctx, dsn, "chinook"); err != nil {
			t.Fatalf("a reader without a role statement_timeout is refused: %v", err)
		}
		t.Setenv("OVDB_PG_CHINOOK_DSN", dsn)
		db, err := mountDemoPostgres(provider)
		if err != nil {
			t.Fatalf("mount: %v", err)
		}
		_ = db.Close()
		t.Setenv(demoPostgresEnabledEnv, "true")
		for _, source := range demoPostgresSources[1:] {
			t.Setenv(source.dsnEnv, "")
		}
		providers, err := appendConfiguredDemoPostgres(nil)
		if err != nil {
			t.Fatal(err)
		}
		handler, closeDatabases, err := newHandlerWithProviders(providers)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = closeDatabases() })
		mounter := handler.(*demoPostgresHandler).mounter
		if err := mounter.waitSettled(ctx); err != nil {
			t.Fatal(err)
		}
		if state, _ := mounter.state("chinook-postgresql"); state != demoPostgresAvailable {
			t.Fatalf("chinook-postgresql is %s: %+v", state, mounter.snapshot())
		}
		response := postgreSQLRequest(t, handler, http.MethodPost, "/v1/databases/chinook-postgresql/tugql", "from: {schema: chinook, name: ovdb_it_session}\ncolumns: [{field: statement_timeout}, {field: read_only}]\n")
		if response.Code != http.StatusOK {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
		var answer struct {
			Records []map[string]any `json:"records"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || len(answer.Records) != 1 {
			t.Fatalf("session settings: %v %s", err, response.Body.String())
		}
		got := fmt.Sprint(answer.Records[0])
		if !strings.Contains(got, "statement_timeout:15s") || !strings.Contains(got, "read_only:on") {
			t.Fatalf("mounted session settings = %s, want statement_timeout 15s and a read-only transaction", got)
		}
	})

	t.Run("the library refuses a role this service's self-check passes", func(t *testing.T) {
		t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP FUNCTION IF EXISTS chinook.ovdb_it_definer()") })
		exec("DROP FUNCTION IF EXISTS chinook.ovdb_it_definer()")
		exec("CREATE FUNCTION chinook.ovdb_it_definer() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'")
		dsn := newReader("ovdb_it_definer")
		if err := checkDemoPostgresSource(ctx, dsn, "chinook"); err != nil {
			t.Fatalf("self-check: %v (the scenario needs a role only the library refuses)", err)
		}
		t.Setenv("OVDB_PG_CHINOOK_DSN", dsn)
		db, err := mountDemoPostgres(provider)
		if err == nil {
			_ = db.Close()
			t.Fatal("the library mounted a role that may execute a SECURITY DEFINER function")
		}
		if !strings.Contains(err.Error(), "not read-only") || strings.Contains(err.Error(), dsn) {
			t.Fatalf("library refusal = %v", err)
		}
		if code := demoPostgresReasonCode(fmt.Errorf("%w: %w", errDemoPostgresMountFailed, err)); code != "role_unsafe" {
			t.Fatalf("status reason of the library refusal = %q, want role_unsafe", code)
		}
	})
}
