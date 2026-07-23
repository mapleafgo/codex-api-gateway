package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFileRollsBySize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	rf, err := openRotatingFile(logPath, 1, 2) // 1 MiB threshold, but we'll force small by writing after hacking maxSize
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 测试用：把阈值降到 64 字节，便于快速滚动
	rf.maxSize = 64
	defer rf.Close()

	line := strings.Repeat("a", 40) + "\n"
	for i := 0; i < 5; i++ {
		if _, err := rf.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	_ = rf.Close()

	// 应产生当前文件 + .1（可能还有 .2）
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("backup .1 missing: %v", err)
	}
}
