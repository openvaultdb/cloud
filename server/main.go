package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func main() {
	if len(os.Args) > 1 {
		if err := runSourceFreeProbe(os.Args[1:], os.Stdout); err != nil {
			log.Fatal("source-free probe failed")
		}
		return
	}
	databases, handler, closeDatabases, err := configuredHandler()
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
	return newHandlerWithStorage(providers, sealedCopy, nil)
}

func newHandlerWithStorage(providers []runtimeDatabase, strategy selectedStorage, image *protectedImage) (http.Handler, func() error, error) {
	if strategy != sealedCopy && strategy != protectedImageStorage {
		return nil, nil, errors.New("unsupported selected storage strategy")
	}
	if strategy == protectedImageStorage && image == nil {
		return nil, nil, errors.New("protected-image startup proof is missing")
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	if len(providers) == 0 {
		return nil, nil, errors.New("no sample database manifests configured")
	}
	databases := make(map[string]*core.Database, len(providers))
	corsOrigins := make(map[string]struct{})
	profiles := map[string]server.ReadProfile{}
	for _, provider := range providers {
		id, manifest := provider.ID, provider.Manifest
		if provider.ReadProfile != "" || provider.ServingAdapter != "" || provider.PublisherManifest != nil || provider.PublicDescriptor != nil {
			if err := validateRuntimeMode(provider, 2); err != nil {
				return nil, nil, errors.Join(err, closeMountedDatabases(databases))
			}
		}
		if manifest == "" {
			return nil, nil, errors.Join(fmt.Errorf("manifest path for database %q is empty", id), closeMountedDatabases(databases))
		}
		if _, exists := databases[id]; exists {
			return nil, nil, errors.Join(fmt.Errorf("database ID %q is configured more than once", id), closeMountedDatabases(databases))
		}
		var database *core.Database
		var err error
		if provider.ReadProfile != "" {
			if strategy == protectedImageStorage {
				database, err = mountSelectedImage(provider, image)
			} else {
				database, err = mountSelectedSnapshot(provider)
			}
			profiles[id] = server.ReadProfile{Kind: server.BoundedImmutable, AllowOrdinaryQuery: true, PublishedQuery: provider.RequirePublishedQuery != nil && *provider.RequirePublishedQuery}
		} else {
			database, err = mount.File(manifest)
		}
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
	cors := server.ParseCORSOrigins(origins)
	if cors != nil {
		pins := []string{"OVDB-Provider-Revision", "OVDB-Source-SHA256", "OVDB-Serving-SHA256", "OVDB-Manifest-SHA256"}
		var err error
		cors, err = cors.WithHeaders(pins, pins)
		if err != nil {
			return nil, nil, errors.Join(err, closeMountedDatabases(databases))
		}
	}
	options := append(cloudServerOptions(),
		server.WithReadOnly(true),
		server.WithDatabaseReadProfiles(profiles),
		server.WithPublicOrigin("https://cloud.openvaultdb.com"),
		server.WithCORS(cors),
	)
	checked, err := server.NewChecked("demodb-cloud", databases, options...)
	if err != nil {
		return nil, nil, errors.Join(err, closeMountedDatabases(databases))
	}
	handler := checked.Handler()
	return handler, func() error { return closeMountedDatabases(databases) }, nil
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
