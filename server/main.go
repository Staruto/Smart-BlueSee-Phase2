package main

import (
	"log"
	"net/http"
)

func main() {
	cfg := loadConfig()

	app, err := newServerApp(cfg)
	if err != nil {
		log.Fatalf("create server app: %v", err)
	}

	if err := app.start(); err != nil {
		log.Fatalf("start server app: %v", err)
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)

	log.Printf("HTTP server listening on %s", cfg.httpAddr)
	if err := http.ListenAndServe(cfg.httpAddr, mux); err != nil {
		log.Fatal(err)
	}
}
