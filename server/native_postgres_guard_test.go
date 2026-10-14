package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestDemoPostgresDSNRequiresVerifiedTLS(t *testing.T) {
	for _, c := range []struct {
		name     string
		dsn      string
		insecure bool
		want     error
	}{
		{"URL verify-full", "postgres://reader:pw@ep-x.eu-central-1.aws.neon.tech/chinook?sslmode=verify-full", false, nil},
		{"URL verify-ca", "postgresql://reader:pw@ep-x.neon.tech/chinook?sslmode=verify-ca&connect_timeout=10", false, nil},
		{"keyword verify-full", "host=ep-x.neon.tech user=reader password='p w' dbname=chinook sslmode=verify-full", false, nil},
		{"URL require", "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=require", false, errDemoPostgresTLS},
		{"URL without sslmode", "postgres://reader:pw@ep-x.neon.tech/chinook", false, errDemoPostgresTLS},
		{"keyword disable", "host=ep-x.neon.tech sslmode=disable", false, errDemoPostgresTLS},
		{"repeated sslmode keeps the last", "postgres://r@h/db?sslmode=verify-full&sslmode=prefer", false, errDemoPostgresTLS},
		{"insecure flag on a remote host", "postgres://reader:pw@ep-x.neon.tech/chinook?sslmode=disable", true, errDemoPostgresInsecureHost},
		{"insecure flag on one remote host of two", "postgres://r@127.0.0.1:5432,ep-x.neon.tech:5432/db?sslmode=disable", true, errDemoPostgresInsecureHost},
		{"insecure flag on loopback", "postgres://reader@127.0.0.1:55440/chinook?sslmode=disable", true, nil},
		{"insecure flag on IPv6 loopback", "postgres://reader@[::1]:55440/chinook?sslmode=disable", true, nil},
		{"insecure flag on localhost keywords", "host=localhost port=5432 sslmode=disable", true, nil},
		{"options parameter", "postgres://r@h/db?sslmode=verify-full&options=-c%20default_transaction_read_only%3Doff", false, errDemoPostgresOptionsParam},
		{"unparseable", "postgres://%zz", false, errDemoPostgresDSNInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := validateDemoPostgresDSN(c.dsn, c.insecure); !errors.Is(err, c.want) {
				t.Fatalf("validateDemoPostgresDSN = %v, want %v", err, c.want)
			}
		})
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
	safe := demoPostgresRoleFacts{Role: "chinook_reader", StatementTimeoutMS: 10_000, ReadableSchemas: []string{"chinook"}}
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
		return demoPostgresRoleFacts{Role: "chinook_reader", StatementTimeoutMS: 5000, ReadableSchemas: []string{"chinook"}}, nil
	}
	t.Setenv(demoPostgresInsecureTLSEnv, "")
	if err := checkDemoPostgresSource(context.Background(), "postgres://r@ep-x.neon.tech/chinook?sslmode=require", "chinook"); !errors.Is(err, errDemoPostgresTLS) || connected {
		t.Fatalf("unverified TLS: err=%v connected=%v", err, connected)
	}
	if err := checkDemoPostgresSource(context.Background(), "postgres://r@ep-x.neon.tech/chinook?sslmode=verify-full", "chinook"); err != nil || !connected {
		t.Fatalf("verified TLS: err=%v connected=%v", err, connected)
	}
	if err := checkDemoPostgresSource(context.Background(), "postgres://r@ep-x.neon.tech/northwind?sslmode=verify-full", "northwind"); !errors.Is(err, errDemoPostgresSchemaScope) {
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
	dsn := "postgres://reader:hunter2-secret@ep-x.neon.tech/chinook?sslmode=verify-full"
	err := redactDemoPostgresError(errors.New("dial "+dsn+" failed for hunter2-secret"), dsn)
	if strings.Contains(err.Error(), "hunter2-secret") || strings.Contains(err.Error(), "ep-x.neon.tech") {
		t.Fatalf("redacted error still discloses the DSN: %v", err)
	}
	keyword := "host=h user=r password=topsecretvalue sslmode=verify-full"
	if err := redactDemoPostgresError(errors.New("bad password topsecretvalue"), keyword); strings.Contains(err.Error(), "topsecretvalue") {
		t.Fatalf("keyword password disclosed: %v", err)
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
