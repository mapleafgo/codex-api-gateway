package main

import (
	"flag"
	"log"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	srv := server.New(cfg)
	log.Printf("codex-api-gateway listening on %s", cfg.Server.Listen)
	if err := httpListenAndServe(cfg.Server.Listen, srv.Handler()); err != nil {
		log.Fatalf("server: %v", err)
	}
}
