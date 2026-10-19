package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
)

func browserURL(host string, port int) string {
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port))}).String()
}

func openBrowser(ctx context.Context, address string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", address)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", address)
	default:
		return fmt.Errorf("opening a browser is unsupported on %s", runtime.GOOS)
	}
	return cmd.Run()
}
