package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyDownloadHashLogsHash(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}

	path := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	a.verifyDownloadHash(logf, path)

	found := false
	for _, l := range logged {
		if strings.HasPrefix(l, "sha256 ") && strings.Contains(l, "a.mkv") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a sha256 log line, got %v", logged)
	}
}

func TestVerifyDownloadHashWarnsOnDuplicateContent(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}

	if err := os.WriteFile(filepath.Join(dir, "original.mkv"), []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}
	dup := filepath.Join(dir, "copy.mkv")
	if err := os.WriteFile(dup, []byte("same content"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	a.verifyDownloadHash(logf, dup)

	found := false
	for _, l := range logged {
		if strings.Contains(l, "identical") && strings.Contains(l, "original.mkv") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a duplicate-content warning, got %v", logged)
	}
}

func TestVerifyDownloadHashNoWarningForDistinctContent(t *testing.T) {
	dir := t.TempDir()
	a := &App{config: filepath.Join(dir, "config.json"), cfg: AppConfig{Settings: Settings{FinishedDir: dir}}}

	if err := os.WriteFile(filepath.Join(dir, "one.mkv"), []byte("content one"), 0o644); err != nil {
		t.Fatal(err)
	}
	two := filepath.Join(dir, "two.mkv")
	if err := os.WriteFile(two, []byte("content two"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	a.verifyDownloadHash(logf, two)

	for _, l := range logged {
		if strings.Contains(l, "identical") {
			t.Fatalf("did not expect a duplicate warning, got %v", logged)
		}
	}
}
