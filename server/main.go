package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func main() {
	manifest := os.Getenv("CHINOOK_MANIFEST")
	if manifest == "" {
		manifest = "/srv/fixture/chinook.yaml"
	}
	handler, closeDatabase, err := newHandler(manifest)
	if err != nil {
		log.Fatalf("mount Chinook: %v", err)
	}
	defer closeDatabase()

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
	log.Printf("serving read-only Chinook OVDB on port %s", port)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve Chinook: %v", err)
	}
}

func newHandler(manifest string) (http.Handler, func() error, error) {
	database, err := mount.File(manifest)
	if err != nil {
		return nil, nil, err
	}
	handler := server.New("chinook-cloud", map[string]*core.Database{"chinook": database},
		server.WithReadOnly(true),
		server.WithPublicOrigin("https://cloud.openvaultdb.com"),
		server.WithCORS(server.ParseCORSOrigins([]string{"https://chinookdb.com", "https://www.chinookdb.com"})),
		server.WithReadCacheTTL(24*time.Hour),
	).Handler()
	return handler, database.Close, nil
}
