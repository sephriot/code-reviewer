// Package localnotify provides fixed-command, machine-local notification
// adapters. It never shells out, makes network calls, or accepts an executable
// path from configuration.
package localnotify

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/sephriot/code-reviewer/internal/application/notificationworker"
)

const defaultMacOSSound = "/System/Library/Sounds/Glass.aiff"

// Local uses supported OS-native commands for sound and speech. Unsupported
// platforms return the worker's explicit unavailable sentinel.
type Local struct {
	GOOS string
	Run  func(context.Context, string, ...string) error
}

// PlaySound plays a configured local file, or a fixed macOS default sound.
func (l Local) PlaySound(ctx context.Context, path string) error {
	if l.goos() != "darwin" {
		return notificationworker.ErrLocalNotifierUnavailable
	}
	if strings.TrimSpace(path) == "" {
		path = defaultMacOSSound
	}
	return l.run(ctx, "afplay", path)
}

// Speak announces a bounded event summary at a user preference-derived rate.
func (l Local) Speak(ctx context.Context, message string, rateMilli int) error {
	if l.goos() != "darwin" {
		return notificationworker.ErrLocalNotifierUnavailable
	}
	if rateMilli < 500 || rateMilli > 2000 || strings.TrimSpace(message) == "" {
		return errors.New("local speech input is invalid")
	}
	return l.run(ctx, "say", "-r", strconv.Itoa(rateMilli/10), message)
}

func (l Local) goos() string {
	if l.GOOS != "" {
		return l.GOOS
	}
	return runtime.GOOS
}

func (l Local) run(ctx context.Context, name string, arguments ...string) error {
	if l.Run != nil {
		return l.Run(ctx, name, arguments...)
	}
	if err := exec.CommandContext(ctx, name, arguments...).Run(); err != nil {
		return err
	}
	return nil
}
