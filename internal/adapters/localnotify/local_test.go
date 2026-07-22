package localnotify

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sephriot/code-reviewer/internal/application/notificationworker"
)

func TestLocalUsesFixedMacOSCommands(t *testing.T) {
	var name string
	var arguments []string
	local := Local{GOOS: "darwin", Run: func(_ context.Context, gotName string, gotArguments ...string) error {
		name, arguments = gotName, gotArguments
		return nil
	}}
	if err := local.PlaySound(context.Background(), ""); err != nil || name != "afplay" || !reflect.DeepEqual(arguments, []string{defaultMacOSSound}) {
		t.Fatalf("sound name=%q arguments=%v err=%v", name, arguments, err)
	}
	if err := local.Speak(context.Background(), "Code review notification: policy.evaluated", 1250); err != nil || name != "say" || !reflect.DeepEqual(arguments, []string{"-r", "125", "Code review notification: policy.evaluated"}) {
		t.Fatalf("speech name=%q arguments=%v err=%v", name, arguments, err)
	}
}

func TestLocalSuppressesUnsupportedPlatformAndRejectsUnsafeSpeech(t *testing.T) {
	local := Local{GOOS: "linux"}
	if err := local.PlaySound(context.Background(), "sound.wav"); !errors.Is(err, notificationworker.ErrLocalNotifierUnavailable) {
		t.Fatalf("sound err=%v", err)
	}
	if err := local.Speak(context.Background(), "message", 1000); !errors.Is(err, notificationworker.ErrLocalNotifierUnavailable) {
		t.Fatalf("speech err=%v", err)
	}
	local.GOOS = "darwin"
	if err := local.Speak(context.Background(), "", 1000); err == nil {
		t.Fatal("empty speech accepted")
	}
}
