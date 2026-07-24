package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureLogging(t *testing.T) {
	oldWriter := log.Writer()
	log.SetOutput(os.Stderr)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	logPath := filepath.Join(t.TempDir(), "logs", "code-reviewer.log")
	cleanup, err := configureLogging(logPath)
	if err != nil {
		t.Fatalf("configureLogging() error = %v", err)
	}
	log.Print("runner test message")
	cleanup()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "runner test message") {
		t.Fatalf("log file does not contain test message: %q", data)
	}
}
