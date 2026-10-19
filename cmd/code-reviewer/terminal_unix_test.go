//go:build darwin || linux

package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestReadSpacebar(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan struct{}, 10)
	done := make(chan error, 1)
	go func() {
		done <- readSpacebar(ctx, int(reader.Fd()), func() { opened <- struct{}{} })
	}()
	if _, err := writer.WriteString("a\n\t "); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("space did not open browser without Enter")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keyboard listener did not stop while idle")
	}
	if len(opened) != 0 {
		t.Fatal("non-space keys opened the browser")
	}
}

func TestReadSpacebarEOF(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer.Close()
	done := make(chan error, 1)
	go func() {
		done <- readSpacebar(context.Background(), int(reader.Fd()), func() {
			t.Error("EOF opened browser")
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keyboard listener did not stop at EOF")
	}
}
