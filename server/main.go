package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func main() {
	databases, err := configuredDatabases()
	if err != nil {
		log.Fatalf("load sample database inventory: %v", err)
	}
	handler, closeDatabases, err := newHandlerWithProviders(databases)
	if err != nil {
		log.Fatalf("mount sample databases: %v", err)
	}
	defer func() {
		if err := closeDatabases(); err != nil {
			log.Printf("close sample databases: %v", err)
		}
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	httpServer := &http.Server{Addr: ":" + port, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-stop.Done()
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("serving read-only sample databases %v on port %s", databaseIDs(databases), port)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve sample databases: %v", err)
	}
}

func newHandler(manifest string) (http.Handler, func() error, error) {
	return newHandlerWithManifests(map[string]string{"chinook": manifest})
}

func newHandlerWithManifests(manifests map[string]string) (http.Handler, func() error, error) {
	databases := make([]runtimeDatabase, 0, len(manifests))
	for id, manifest := range manifests {
		databases = append(databases, runtimeDatabase{ID: id, Manifest: manifest, CORSOrigins: legacyCORSOrigins(id)})
	}
	return newHandlerWithProviders(databases)
}

func newHandlerWithProviders(providers []runtimeDatabase) (http.Handler, func() error, error) {
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	if len(providers) == 0 {
		return nil, nil, errors.New("no sample database manifests configured")
	}
	databases := make(map[string]*core.Database, len(providers))
	quotedCollections := make(map[string]map[string]string, len(providers))
	corsOrigins := make(map[string]struct{})
	for _, provider := range providers {
		id, manifest := provider.ID, provider.Manifest
		if manifest == "" {
			return nil, nil, errors.Join(fmt.Errorf("manifest path for database %q is empty", id), closeMountedDatabases(databases))
		}
		if _, exists := databases[id]; exists {
			return nil, nil, errors.Join(fmt.Errorf("database ID %q is configured more than once", id), closeMountedDatabases(databases))
		}
		database, err := mount.File(manifest)
		if err != nil {
			return nil, nil, errors.Join(err, closeMountedDatabases(databases))
		}
		if database.Manifest.Database.ID != id {
			return nil, nil, errors.Join(
				fmt.Errorf("manifest %q declares database ID %q, want %q", manifest, database.Manifest.Database.ID, id),
				database.Close(),
				closeMountedDatabases(databases),
			)
		}
		if !runtimeDatabaseIDPattern.MatchString(id) {
			return nil, nil, errors.Join(fmt.Errorf("invalid database ID %q", id), database.Close(), closeMountedDatabases(databases))
		}
		quotedCollections[id] = quotedCollectionAliases(database)
		normalizeManifestIdentifiers(database)
		databases[id] = database
		for _, origin := range provider.CORSOrigins {
			corsOrigins[origin] = struct{}{}
		}
	}
	origins := make([]string, 0, len(corsOrigins))
	for origin := range corsOrigins {
		origins = append(origins, origin)
	}
	sort.Strings(origins)
	handler := server.New("demodb-cloud", databases,
		server.WithReadOnly(true),
		server.WithPublicOrigin("https://cloud.openvaultdb.com"),
		server.WithCORS(server.ParseCORSOrigins(origins)),
	).Handler()
	return withQuotedCollectionRecordReads(handler, quotedCollections), func() error { return closeMountedDatabases(databases) }, nil
}

func closeMountedDatabases(databases map[string]*core.Database) error {
	var closeErr error
	for _, database := range databases {
		if err := database.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func quotedCollectionAliases(database *core.Database) map[string]string {
	aliases := make(map[string]string)
	if database.Manifest.Schemas == nil {
		return aliases
	}
	for name := range database.Manifest.Schemas.Collections {
		logical := logicalIdentifier(name)
		if logical != name {
			aliases[logical] = name
		}
	}
	return aliases
}

func withQuotedCollectionRecordReads(next http.Handler, aliases map[string]map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			segments := strings.Split(r.URL.EscapedPath(), "/")
			if len(segments) >= 7 && segments[1] == "v1" && segments[2] == "databases" && segments[4] == "records" {
				databaseID, err := url.PathUnescape(segments[3])
				if err == nil {
					collection, decodeErr := url.PathUnescape(segments[5])
					if decodeErr == nil {
						if quoted := aliases[databaseID][collection]; quoted != "" {
							rewriteEscapedPath(r, segments, 5, quoted)
						}
					}
				}
			} else if r.Method == http.MethodGet && len(segments) == 5 && segments[1] == "v1" && segments[2] == "databases" && segments[4] == "read" {
				databaseID, err := url.PathUnescape(segments[3])
				if err == nil {
					params := r.URL.Query()
					keyParts := strings.Split(params.Get("key"), "/")
					if len(keyParts) >= 2 {
						collection, decodeErr := url.PathUnescape(keyParts[0])
						if decodeErr == nil {
							if quoted := aliases[databaseID][collection]; quoted != "" {
								keyParts[0] = url.PathEscape(quoted)
								params.Set("key", strings.Join(keyParts, "/"))
								r.URL.RawQuery = params.Encode()
							}
						}
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func rewriteEscapedPath(r *http.Request, segments []string, index int, identifier string) {
	segments[index] = url.PathEscape(identifier)
	rawPath := strings.Join(segments, "/")
	if decodedPath, err := url.PathUnescape(rawPath); err == nil {
		r.URL.Path = decodedPath
		r.URL.RawPath = rawPath
	}
}

func normalizeManifestIdentifiers(database *core.Database) {
	if database.Manifest.Schemas == nil {
		return
	}
	collections := make(map[string]schema.Collection, len(database.Manifest.Schemas.Collections))
	for name, collection := range database.Manifest.Schemas.Collections {
		logicalName := logicalIdentifier(name)
		fields := make(map[string]schema.Field, len(collection.Fields))
		for field, definition := range collection.Fields {
			fields[logicalIdentifier(field)] = definition
		}
		collection.Fields = fields
		collections[logicalName] = collection
	}
	database.Manifest.Schemas.Collections = collections
}

func logicalIdentifier(name string) string {
	if len(name) < 2 || name[0] != '"' || name[len(name)-1] != '"' {
		return name
	}
	return strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
}
