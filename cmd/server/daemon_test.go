package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestStripDaemonFlags(t *testing.T) {
	got := stripDaemonFlags([]string{"-config", "config.yaml", "-d", "--other"})
	want := []string{"-config", "config.yaml", "--other"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	got = stripDaemonFlags([]string{"--daemon", "-d=true", "x"})
	want = []string{"x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestResolveDaemonPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := resolveDaemonPath("", "gateway.pid")
	if got != filepath.Join(wd, "gateway.pid") {
		t.Fatalf("default rel: got %q", got)
	}
	got = resolveDaemonPath("/tmp/x.pid", "gateway.pid")
	if got != "/tmp/x.pid" {
		t.Fatalf("abs: got %q", got)
	}
}

func TestWaitDaemonReadySuccess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "gateway.pid")
	pid := os.Getpid()
	waitDone := make(chan error)
	// 延迟写入 pid，模拟监听成功
	go func() {
		time.Sleep(80 * time.Millisecond)
		if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
			t.Errorf("write pid: %v", err)
		}
	}()
	if err := waitDaemonReady(pid, pidPath, waitDone, time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

func TestWaitDaemonReadyChildExit(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "gateway.pid")
	waitDone := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		waitDone <- nil
	}()
	err := waitDaemonReady(os.Getpid(), pidPath, waitDone, time.Second)
	if err == nil {
		t.Fatal("expected error when child exits")
	}
}

func TestWaitDaemonReadyTimeout(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "gateway.pid")
	waitDone := make(chan error) // never closes
	err := waitDaemonReady(os.Getpid(), pidPath, waitDone, 120*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
}
