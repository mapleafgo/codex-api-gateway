package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func TestConfigureAndRollOver(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	// 1 MiB 阈值 + 2 个备份
	cfg := config.LoggingCfg{
		File:       logPath,
		Level:      "info",
		Format:     "text",
		MaxSizeMB:  1,
		MaxBackups: 2,
	}
	if err := Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// 写入足够多的数据触发滚动（约 1.2 MiB）
	line := strings.Repeat("x", 1024) + "\n"
	for i := 0; i < 1200; i++ {
		_, _ = currentSink.Write([]byte(line))
	}

	// 关闭以刷盘
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 验证：当前文件和至少一个备份存在
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("backup .1 missing: %v", err)
	}
}

func TestConfigureStderrNoOp(t *testing.T) {
	cfg := config.LoggingCfg{
		File:   "",
		Level:  "debug",
		Format: "text",
	}
	if err := Configure(cfg); err != nil {
		t.Fatalf("Configure stderr: %v", err)
	}
	// Close 在 stderr 模式应为 no-op
	if err := Close(); err != nil {
		t.Fatalf("Close stderr: %v", err)
	}
}

func TestConfigureIdempotent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	cfg := config.LoggingCfg{
		File:   logPath,
		Level:  "info",
		Format: "text",
	}
	if err := Configure(cfg); err != nil {
		t.Fatalf("first Configure: %v", err)
	}
	// 同路径再次 Configure 应复用同一 sink（不创建新 AsyncWriter）
	if err := Configure(cfg); err != nil {
		t.Fatalf("second Configure: %v", err)
	}
	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestConfigurePathSwitch(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "app1.log")
	path2 := filepath.Join(dir, "app2.log")

	cfg1 := config.LoggingCfg{File: path1, Level: "info", Format: "text", MaxSizeMB: 1, MaxBackups: 1}
	if err := Configure(cfg1); err != nil {
		t.Fatalf("Configure path1: %v", err)
	}
	_, _ = currentSink.Write([]byte("path1\n"))

	// 等待异步刷盘完成后再切换路径，否则旧队列数据会因 sink 替换而丢失
	if currentAW != nil {
		if err := currentAW.Sync(); err != nil {
			t.Fatalf("Sync path1: %v", err)
		}
	}

	cfg2 := config.LoggingCfg{File: path2, Level: "info", Format: "text", MaxSizeMB: 1, MaxBackups: 1}
	if err := Configure(cfg2); err != nil {
		t.Fatalf("Configure path2: %v", err)
	}
	_, _ = currentSink.Write([]byte("path2\n"))

	if err := Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 两个文件都应有内容
	if info, err := os.Stat(path1); err != nil || info.Size() == 0 {
		t.Fatalf("path1 should have content: size=%d err=%v", func() int64 {
			if err != nil {
				return 0
			}
			return info.Size()
		}(), err)
	}
	if info, err := os.Stat(path2); err != nil || info.Size() == 0 {
		t.Fatalf("path2 should have content: size=%d err=%v", func() int64 {
			if err != nil {
				return 0
			}
			return info.Size()
		}(), err)
	}
}
