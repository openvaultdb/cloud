package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const defaultRuntimeInventoryPath = "/srv/fixture/inventory.json"

var runtimeDatabaseIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var providerRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var providerRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var providerSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type runtimeInventory struct {
	Version   int               `json:"version"`
	Databases []runtimeDatabase `json:"databases"`
}

type runtimeDatabase struct {
	ID                 string   `json:"id"`
	Manifest           string   `json:"manifest"`
	CORSOrigins        []string `json:"corsOrigins"`
	ProviderRepository string   `json:"providerRepository"`
	ProviderRevision   string   `json:"providerRevision"`
	SourceSHA256       string   `json:"sourceSha256"`
	ServingSHA256      string   `json:"servingSha256"`
	License            string   `json:"license"`
	LicenseSHA256      string   `json:"licenseSha256"`
	SmokeRecordset     string   `json:"smokeRecordset"`
	SmokeRecordsets    []string `json:"smokeRecordsets"`
}

func loadRuntimeInventory(path string) ([]runtimeDatabase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runtime provider inventory %q: %w", path, err)
	}
	var inventory runtimeInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return nil, fmt.Errorf("decode runtime provider inventory %q: %w", path, err)
	}
	if inventory.Version != 1 || len(inventory.Databases) == 0 {
		return nil, fmt.Errorf("runtime provider inventory %q must contain version 1 and at least one database", path)
	}

	seenIDs := make(map[string]struct{}, len(inventory.Databases))
	for _, database := range inventory.Databases {
		if !runtimeDatabaseIDPattern.MatchString(database.ID) {
			return nil, fmt.Errorf("runtime provider inventory has invalid database ID %q", database.ID)
		}
		if _, exists := seenIDs[database.ID]; exists {
			return nil, fmt.Errorf("runtime provider inventory repeats database ID %q", database.ID)
		}
		seenIDs[database.ID] = struct{}{}
	}
	base := filepath.Dir(path)
	for index := range inventory.Databases {
		database := &inventory.Databases[index]
		if !safeInventoryFilename(database.Manifest) || !safeInventoryFilename(database.License) {
			return nil, fmt.Errorf("runtime provider %q manifest and license must be simple filenames", database.ID)
		}
		if !providerRepositoryPattern.MatchString(database.ProviderRepository) || !providerRevisionPattern.MatchString(database.ProviderRevision) || !providerSHA256Pattern.MatchString(database.SourceSHA256) || !providerSHA256Pattern.MatchString(database.ServingSHA256) || !providerSHA256Pattern.MatchString(database.LicenseSHA256) {
			return nil, fmt.Errorf("runtime provider %q is missing immutable source identity", database.ID)
		}
		if len(database.CORSOrigins) == 0 || database.SmokeRecordset == "" {
			return nil, fmt.Errorf("runtime provider %q has no CORS origins or smoke recordset", database.ID)
		}
		if len(database.SmokeRecordsets) == 0 {
			database.SmokeRecordsets = []string{database.SmokeRecordset}
		}
		seenRecordsets := make(map[string]struct{}, len(database.SmokeRecordsets))
		for _, recordset := range database.SmokeRecordsets {
			if strings.TrimSpace(recordset) == "" {
				return nil, fmt.Errorf("runtime provider %q has an empty smoke recordset", database.ID)
			}
			if _, exists := seenRecordsets[recordset]; exists {
				return nil, fmt.Errorf("runtime provider %q repeats smoke recordset %q", database.ID, recordset)
			}
			seenRecordsets[recordset] = struct{}{}
		}
		for _, origin := range database.CORSOrigins {
			parsed, parseErr := url.Parse(origin)
			if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return nil, fmt.Errorf("runtime provider %q has invalid CORS origin %q", database.ID, origin)
			}
		}
		marker, readErr := os.ReadFile(filepath.Join(base, database.ID+".source-sha256"))
		if readErr != nil {
			return nil, fmt.Errorf("runtime provider %q source hash receipt: %w", database.ID, readErr)
		}
		actual := strings.TrimSpace(string(marker))
		if actual != database.SourceSHA256 {
			return nil, fmt.Errorf("runtime provider %q source hash receipt does not match its inventory pin", database.ID)
		}
		fixture, readErr := os.ReadFile(filepath.Join(base, database.ID+".sqlite"))
		if readErr != nil {
			return nil, fmt.Errorf("runtime provider %q serving SQLite: %w", database.ID, readErr)
		}
		actualHash := sha256.Sum256(fixture)
		if hex.EncodeToString(actualHash[:]) != database.ServingSHA256 {
			return nil, fmt.Errorf("runtime provider %q serving SQLite hash does not match its inventory pin", database.ID)
		}
		license, readErr := os.ReadFile(filepath.Join(base, database.License))
		if readErr != nil {
			return nil, fmt.Errorf("runtime provider %q license: %w", database.ID, readErr)
		}
		licenseHash := sha256.Sum256(license)
		if hex.EncodeToString(licenseHash[:]) != database.LicenseSHA256 {
			return nil, fmt.Errorf("runtime provider %q license hash does not match its inventory pin", database.ID)
		}
		database.Manifest = filepath.Join(base, database.Manifest)
		database.License = filepath.Join(base, database.License)
	}
	return inventory.Databases, nil
}

func safeInventoryFilename(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`)
}

func configuredDatabases() ([]runtimeDatabase, error) {
	if explicitPath := os.Getenv("SAMPLE_DATABASES_INVENTORY"); explicitPath != "" {
		return loadRuntimeInventory(explicitPath)
	}
	if _, chinookConfigured := os.LookupEnv("CHINOOK_MANIFEST"); chinookConfigured {
		return legacyEnvironmentDatabases()
	}
	if _, northwindConfigured := os.LookupEnv("NORTHWIND_MANIFEST"); northwindConfigured {
		return legacyEnvironmentDatabases()
	}
	if _, err := os.Stat(defaultRuntimeInventoryPath); err == nil {
		return loadRuntimeInventory(defaultRuntimeInventoryPath)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("check runtime provider inventory: %w", err)
	}
	return legacyEnvironmentDatabases()
}

func legacyEnvironmentDatabases() ([]runtimeDatabase, error) {
	chinookManifest := os.Getenv("CHINOOK_MANIFEST")
	if chinookManifest == "" {
		chinookManifest = "/srv/fixture/chinook.yaml"
	}
	databases := []runtimeDatabase{{ID: "chinook", Manifest: chinookManifest, CORSOrigins: legacyCORSOrigins("chinook")}}
	if northwindManifest, configured := os.LookupEnv("NORTHWIND_MANIFEST"); configured {
		databases = append(databases, runtimeDatabase{ID: "northwind", Manifest: northwindManifest, CORSOrigins: legacyCORSOrigins("northwind")})
	} else if _, err := os.Stat("/srv/fixture/northwind.yaml"); err == nil {
		databases = append(databases, runtimeDatabase{ID: "northwind", Manifest: "/srv/fixture/northwind.yaml", CORSOrigins: legacyCORSOrigins("northwind")})
	}
	return databases, nil
}

func legacyCORSOrigins(databaseID string) []string {
	// Retain the original two-provider environment contract for local callers;
	// production CORS origins come from the provider inventory.
	switch databaseID {
	case "chinook":
		return []string{"https://chinookdb.com", "https://www.chinookdb.com", "https://demodb.dev", "https://www.demodb.dev", "https://chinook.demodb.dev"}
	case "northwind":
		return []string{"https://demodb.dev", "https://www.demodb.dev", "https://northwind.demodb.dev"}
	default:
		return nil
	}
}

func databaseIDs(databases []runtimeDatabase) []string {
	ids := make([]string, 0, len(databases))
	for _, database := range databases {
		ids = append(ids, database.ID)
	}
	sort.Strings(ids)
	return ids
}
