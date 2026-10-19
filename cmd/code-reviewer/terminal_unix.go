//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"log"
	"os"

	"golang.org/x/sys/unix"
)

func startBrowserShortcut(ctx context.Context, host string, port int) func() {
	fd := int(os.Stdin.Fd())
	original, err := unix.IoctlGetTermios(fd, terminalGet)
	if err != nil {
		return func() {}
	}
	state := *original
	state.Lflag &^= unix.ICANON | unix.ECHO
	state.Cc[unix.VMIN] = 1
	state.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, terminalSet, &state); err != nil {
		log.Printf("web: cannot enable spacebar shortcut: %v", err)
		return func() {}
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	address := browserURL(host, port)
	log.Printf("web: press Space to open %s in your browser", address)
	go func() {
		defer close(done)
		defer func() {
			if err := unix.IoctlSetTermios(fd, terminalSet, original); err != nil {
				log.Printf("web: cannot restore terminal: %v", err)
			}
		}()
		if err := readSpacebar(ctx, fd, func() {
			if err := openBrowser(ctx, address); err != nil && ctx.Err() == nil {
				log.Printf("web: cannot open browser: %v", err)
			}
		}); err != nil {
			log.Printf("web: keyboard shortcut error: %v", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func readSpacebar(ctx context.Context, fd int, open func()) error {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var buf [1]byte
	for ctx.Err() == nil {
		_, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return unix.EBADF
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
			continue
		}
		n, err := unix.Read(fd, buf[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n == 0 {
			return err
		}
		if buf[0] == ' ' && ctx.Err() == nil {
			open()
		}
	}
	return nil
}
