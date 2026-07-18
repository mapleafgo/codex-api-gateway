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

	// 两阶段初始化：先只解析 logging 段并配置日志系统，确保后续 config.Load
	// 的日志（含 base_instructions 加载、配置加载完成等）走配置好的 handler，
	// 而不是以 Go 默认格式打到终端。
	loggingCfg := config.LoadLogging(*configPath)
	if err := logging.Configure(loggingCfg); err != nil {
		slog.Error("配置日志失败", "log_file", loggingCfg.File, "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("加载配置失败", "config_path", *configPath, "error", err)
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
