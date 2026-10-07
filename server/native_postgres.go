package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const demoPostgresEnabledEnv = "OVDB_DEMODB_POSTGRES_ENABLED"

type demoPostgresSource struct {
	databaseID string
	dsnEnv     string
}

type nativePostgresMountConfig struct {
	schemaName string
	dsnEnv     string
}

var demoPostgresSources = [...]demoPostgresSource{
	{databaseID: "chinook", dsnEnv: "OVDB_PG_CHINOOK_DSN"},
	{databaseID: "northwind", dsnEnv: "OVDB_PG_NORTHWIND_DSN"},
	{databaseID: "pubs", dsnEnv: "OVDB_PG_PUBS_DSN"},
	{databaseID: "sakila", dsnEnv: "OVDB_PG_SAKILA_DSN"},
	{databaseID: "adventureworks", dsnEnv: "OVDB_PG_ADVENTUREWORKS_DSN"},
	{databaseID: "employees", dsnEnv: "OVDB_PG_EMPLOYEES_DSN"},
}

func appendConfiguredDemoPostgres(providers []runtimeDatabase) ([]runtimeDatabase, error) {
	enabledValue, configured := os.LookupEnv(demoPostgresEnabledEnv)
	if !configured || strings.TrimSpace(enabledValue) == "" {
		return providers, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(enabledValue))
	if err != nil {
		return nil, fmt.Errorf("%s must be true or false", demoPostgresEnabledEnv)
	}
	if !enabled {
		return providers, nil
	}

	seen := make(map[string]struct{}, len(providers)+len(demoPostgresSources))
	for _, provider := range providers {
		seen[provider.ID] = struct{}{}
	}
	for _, source := range demoPostgresSources {
		if value, exists := os.LookupEnv(source.dsnEnv); !exists || value == "" {
			return nil, fmt.Errorf("native PostgreSQL is enabled but %s is not configured", source.dsnEnv)
		}
		id := source.databaseID + "-postgresql"
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("native PostgreSQL database ID %q is already configured", id)
		}
		seen[id] = struct{}{}
		providers = append(providers, runtimeDatabase{
			ID:          id,
			CORSOrigins: demoPostgresCORSOrigins(source.databaseID),
			nativePostgres: &nativePostgresMountConfig{
				schemaName: source.databaseID,
				dsnEnv:     source.dsnEnv,
			},
		})
	}
	return providers, nil
}

func demoPostgresCORSOrigins(databaseID string) []string {
	return []string{
		"https://demodb.dev",
		"https://www.demodb.dev",
		"https://" + databaseID + ".demodb.dev",
		"https://datatug.app",
	}
}

func demoPostgresManifest(provider runtimeDatabase) ([]byte, error) {
	config := provider.nativePostgres
	if config == nil || provider.ID != config.schemaName+"-postgresql" || !runtimeDatabaseIDPattern.MatchString(config.schemaName) || !manifestEnvName(config.dsnEnv) {
		return nil, errors.New("invalid native PostgreSQL runtime configuration")
	}
	return []byte(fmt.Sprintf("database:\n  id: %s\n  schema_mode: strict\nstorage:\n  engine: postgres\n  postgres:\n    dsn_env: %s\n    read_only: true\n", provider.ID, config.dsnEnv)), nil
}

func manifestEnvName(name string) bool {
	if name == "" || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for _, char := range name[1:] {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func demoPostgresMountOptions(provider runtimeDatabase) (mount.Options, error) {
	if _, err := demoPostgresManifest(provider); err != nil {
		return mount.Options{}, err
	}
	return mount.Options{
		// Strict schema mode never loads or observes inferred catalogues. Keep
		// CatalogueDir unset so the temporary manifest directory is not retained
		// as a path that core could later write.
		ExcludedNativePostgresRelations: []schema.NativeCollectionSource{{
			Schema: provider.nativePostgres.schemaName,
			Name:   "_import_manifest",
		}},
	}, nil
}

func mountDemoPostgres(provider runtimeDatabase) (db *core.Database, resultErr error) {
	contents, err := demoPostgresManifest(provider)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "ovdb-demo-postgres-")
	if err != nil {
		return nil, errors.New("create temporary native PostgreSQL manifest directory")
	}
	defer func() {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			var closeErr error
			if db != nil {
				closeErr = db.Close()
				db = nil
			}
			resultErr = errors.Join(resultErr, errors.New("remove temporary native PostgreSQL manifest directory"), removeErr, closeErr)
		}
	}()
	manifestPath := filepath.Join(directory, "manifest.yaml")
	if err := os.WriteFile(manifestPath, contents, 0o600); err != nil {
		return nil, errors.New("write temporary native PostgreSQL manifest")
	}
	options, err := demoPostgresMountOptions(provider)
	if err != nil {
		return nil, err
	}
	db, err = mount.FileWithOptions(manifestPath, options)
	if err != nil {
		return nil, err
	}
	return db, nil
}
