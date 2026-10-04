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
	manifest := os.Getenv("CHINOOK_MANIFEST")
	if manifest == "" {
		manifest = "/srv/fixture/chinook.yaml"
	}
	manifests := map[string]string{"chinook": manifest}
	northwindManifest, northwindConfigured := os.LookupEnv("NORTHWIND_MANIFEST")
	if northwindConfigured {
		manifests["northwind"] = northwindManifest
	} else if _, err := os.Stat("/srv/fixture/northwind.yaml"); err == nil {
		manifests["northwind"] = "/srv/fixture/northwind.yaml"
	}
	handler, closeDatabases, err := newHandlerWithManifests(manifests)
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
	log.Printf("serving read-only sample databases %v on port %s", mapKeys(manifests), port)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve Chinook: %v", err)
	}
}

func newHandler(manifest string) (http.Handler, func() error, error) {
	return newHandlerWithManifests(map[string]string{"chinook": manifest})
}

func newHandlerWithManifests(manifests map[string]string) (http.Handler, func() error, error) {
	databases := make(map[string]*core.Database, len(manifests))
	quotedCollections := make(map[string]map[string]string, len(manifests))
	for id, manifest := range manifests {
		if manifest == "" {
			return nil, nil, fmt.Errorf("manifest path for database %q is empty", id)
		}
		database, err := mount.File(manifest)
		if err != nil {
			for _, mounted := range databases {
				_ = mounted.Close()
			}
			return nil, nil, err
		}
		if database.Manifest.Database.ID != id {
			_ = database.Close()
			for _, mounted := range databases {
				_ = mounted.Close()
			}
			return nil, nil, fmt.Errorf("manifest %q declares database ID %q, want %q", manifest, database.Manifest.Database.ID, id)
		}
		quotedCollections[id] = quotedCollectionAliases(database)
		normalizeManifestIdentifiers(database)
		databases[id] = database
	}
	if len(databases) == 0 {
		return nil, nil, errors.New("no sample database manifests configured")
	}
	handler := server.New("demodb-cloud", databases,
		server.WithReadOnly(true),
		server.WithPublicOrigin("https://cloud.openvaultdb.com"),
		server.WithCORS(server.ParseCORSOrigins([]string{
			"https://chinookdb.com", "https://www.chinookdb.com",
			"https://demodb.dev", "https://www.demodb.dev",
			"https://chinook.demodb.dev", "https://northwind.demodb.dev",
		})),
	).Handler()
	return withQuotedCollectionRecordReads(handler, quotedCollections), func() error {
		var closeErr error
		for _, database := range databases {
			if err := database.Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
		return closeErr
	}, nil
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

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
