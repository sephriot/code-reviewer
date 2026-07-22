// Package notificationdispatch derives durable local notification deliveries
// from one already-committed domain event. It never performs channel effects.
package notificationdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// Store reads local preferences and records immutable notification deliveries.
type Store interface {
	LoadNotificationPreferences(context.Context) (sqlite.NotificationPreferences, error)
	CreateNotificationDelivery(context.Context, sqlite.CreateNotificationDeliveryInput) (sqlite.CreateNotificationDeliveryResult, error)
}

// Scheduler ensures one durable worker job for a notification delivery.
type Scheduler interface {
	Schedule(context.Context, string) (sqlite.EnsureJobResult, error)
}

// Request names an already-committed domain event and its safe notification
// payload. DedupeKey identifies this logical occurrence within each channel.
type Request struct {
	DomainEventID string
	DedupeKey     string
	PayloadJSON   []byte
}

// Result reports deliveries derived from the request. A muted result creates
// no delivery work, preserving the operator's local quiet period.
type Result struct {
	Muted       bool
	DeliveryIDs []string
}

// Service derives enabled local deliveries and schedules their durable jobs.
type Service struct {
	Store     Store
	Scheduler Scheduler
	Now       func() time.Time
}

// Dispatch creates one retained delivery for every enabled known channel then
// ensures its job. Replays are safe because store and scheduler are idempotent.
func (s Service) Dispatch(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("notification dispatch context is required")
	}
	if s.Store == nil || s.Scheduler == nil {
		return Result{}, errors.New("notification dispatch dependencies are required")
	}
	request.DomainEventID = strings.TrimSpace(request.DomainEventID)
	request.DedupeKey = strings.TrimSpace(request.DedupeKey)
	if request.DomainEventID == "" || request.DedupeKey == "" || len(request.DomainEventID) > 512 || len(request.DedupeKey) > 512 {
		return Result{}, errors.New("notification dispatch request is invalid")
	}
	preferences, err := s.Store.LoadNotificationPreferences(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("load notification preferences: %w", err)
	}
	channels, err := parseChannels(preferences.ChannelsJSON)
	if err != nil {
		return Result{}, fmt.Errorf("notification preferences are invalid: %w", err)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if preferences.MutedUntil != nil && preferences.MutedUntil.UTC().After(now) {
		return Result{Muted: true}, nil
	}

	result := Result{}
	for _, channel := range enabledChannels(channels) {
		delivery, err := s.Store.CreateNotificationDelivery(ctx, sqlite.CreateNotificationDeliveryInput{
			DomainEventID: request.DomainEventID, Channel: channel, TemplateVersion: 1,
			DedupeKey: request.DedupeKey, PayloadJSON: request.PayloadJSON, AvailableAt: now, CreatedAt: now,
		})
		if err != nil {
			return Result{}, fmt.Errorf("create %s notification delivery: %w", channel, err)
		}
		if _, err := s.Scheduler.Schedule(ctx, delivery.ID); err != nil {
			return Result{}, fmt.Errorf("schedule %s notification delivery: %w", channel, err)
		}
		result.DeliveryIDs = append(result.DeliveryIDs, delivery.ID)
	}
	return result, nil
}

type channelPreferences struct {
	Browser bool `json:"browser"`
	Log     bool `json:"log"`
	Sound   bool `json:"sound"`
	TTS     bool `json:"tts"`
}

func parseChannels(raw []byte) (channelPreferences, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return channelPreferences{}, errors.New("channels are not an object")
	}
	decode := func(key string) (bool, error) {
		value, found := values[key]
		if !found {
			return false, errors.New("known channel is absent")
		}
		var enabled bool
		if err := json.Unmarshal(value, &enabled); err != nil {
			return false, errors.New("known channel is not boolean")
		}
		return enabled, nil
	}
	browser, err := decode("browser")
	if err != nil {
		return channelPreferences{}, err
	}
	log, err := decode("log")
	if err != nil {
		return channelPreferences{}, err
	}
	sound, err := decode("sound")
	if err != nil {
		return channelPreferences{}, err
	}
	tts, err := decode("tts")
	if err != nil {
		return channelPreferences{}, err
	}
	return channelPreferences{Browser: browser, Log: log, Sound: sound, TTS: tts}, nil
}

func enabledChannels(channels channelPreferences) []sqlite.NotificationChannel {
	result := make([]sqlite.NotificationChannel, 0, 4)
	if channels.Browser {
		result = append(result, sqlite.NotificationChannelBrowser)
	}
	if channels.Log {
		result = append(result, sqlite.NotificationChannelLog)
	}
	if channels.Sound {
		result = append(result, sqlite.NotificationChannelSound)
	}
	if channels.TTS {
		result = append(result, sqlite.NotificationChannelTTS)
	}
	return result
}
