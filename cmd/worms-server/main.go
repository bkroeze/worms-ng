package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"worms.ng/internal/server"
)

// assets are compiled into the server. make build embeds the generated WASM
// artifact alongside index.html and the loader script.
//
//go:embed web
var assets embed.FS

func main() {
	address := flag.String("addr", ":8080", "HTTP listen address")
	databasePath := flag.String("db", "worms.db", "SQLite database path")
	corsOrigins := flag.String("cors-origin", "", "comma-separated CORS origins (or * for all)")
	flag.Parse()

	static, err := fs.Sub(assets, "web")
	if err != nil {
		log.Fatalf("static assets: %v", err)
	}
	service, err := server.OpenWithOptions(*databasePath, static, server.Options{CORSOrigins: strings.FieldsFunc(*corsOrigins, func(r rune) bool { return r == ',' || r == ' ' })})
	if err != nil {
		log.Fatalf("open service: %v", err)
	}
	defer service.Close()

	httpServer := &http.Server{Addr: *address, Handler: service.Handler()}
	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stopped
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()
	log.Printf("worms server listening on http://%s", *address)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
