package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingFile 是按大小滚动的日志文件写入器。
// 仅用于本机防误伤：避免 gateway.log 无限膨胀；不做压缩、不做按日期切分。
type rotatingFile struct {
	mu         sync.Mutex
	path       string
	maxSize    int64 // bytes
	maxBackups int
	size       int64
	file       *os.File
}

func openRotatingFile(path string, maxSizeMB, maxBackups int) (*rotatingFile, error) {
	if maxSizeMB <= 0 {
		maxSizeMB = 50
	}
	if maxBackups < 0 {
		maxBackups = 0
	}
	rf := &rotatingFile{
		path:       path,
		maxSize:    int64(maxSizeMB) << 20,
		maxBackups: maxBackups,
	}
	if err := rf.openExistingOrCreate(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (r *rotatingFile) openExistingOrCreate() error {
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("logging: 无法创建日志目录 %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("logging: 无法打开日志文件 %s: %w", r.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logging: 无法 stat 日志文件 %s: %w", r.path, err)
	}
	r.file = f
	r.size = info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return 0, fmt.Errorf("logging: 日志文件已关闭")
	}
	// 写前滚动：若当前 size + 本次写入会超过阈值，先轮转。
	// 单条日志本身超过阈值时仍写出，避免吞日志。
	if r.maxSize > 0 && r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *rotatingFile) rotateLocked() error {
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	// 从最旧备份向前挪：.N 删除，.(i) -> .(i+1)，当前 -> .1
	if r.maxBackups > 0 {
		oldest := fmt.Sprintf("%s.%d", r.path, r.maxBackups)
		_ = os.Remove(oldest)
		for i := r.maxBackups - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", r.path, i)
			dst := fmt.Sprintf("%s.%d", r.path, i+1)
			if _, err := os.Stat(src); err == nil {
				_ = os.Rename(src, dst)
			}
		}
		_ = os.Rename(r.path, r.path+".1")
	} else {
		// 不保留备份：直接截断重开
		_ = os.Remove(r.path)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("logging: 滚动后无法打开日志文件 %s: %w", r.path, err)
	}
	r.file = f
	r.size = 0
	return nil
}
