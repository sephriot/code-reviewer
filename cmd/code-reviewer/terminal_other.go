//go:build !darwin && !linux

package main

import "context"

func startBrowserShortcut(context.Context, string, int) func() {
	return func() {}
}
