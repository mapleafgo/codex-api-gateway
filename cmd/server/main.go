// Package main starts the CodexApiGateway HTTP server.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/server"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("加载配置失败", "config_path", *configPath, "error", err)
		os.Exit(1)
	}
	if err := logging.Configure(cfg.Logging); err != nil {
		slog.Error("配置日志失败", "log_file", cfg.Logging.File, "error", err)
		os.Exit(1)
	}

	srv := server.New(cfg)
	defer srv.Close()
	slog.Info("codex-api-gateway 开始监听", "listen", cfg.Server.Listen, "log_level", cfg.Logging.Level, "log_format", cfg.Logging.Format)
	if err := http.ListenAndServe(cfg.Server.Listen, srv.Handler()); err != nil {
		slog.Error("HTTP 服务退出", "listen", cfg.Server.Listen, "error", err)
		os.Exit(1)
	}
}
