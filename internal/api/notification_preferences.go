package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const notificationPreferencesPath = "/api/v1/notification-preferences"

// NotificationPreferencesStore is deliberately limited to local, mutable
// delivery preferences. It has no provider, job, or delivery capability.
type NotificationPreferencesStore interface {
	LoadNotificationPreferences(context.Context) (sqlite.NotificationPreferences, error)
	UpdateNotificationPreferences(context.Context, sqlite.UpdateNotificationPreferencesInput) (sqlite.NotificationPreferences, error)
}

// NotificationPreferencesOptions supplies local preference persistence. The
// mutation route must be mounted behind MutationAuth.Wrap by its caller.
type NotificationPreferencesOptions struct {
	Store NotificationPreferencesStore
	Now   func() time.Time
}

// NewNotificationPreferencesHandler exposes a loopback-only safe preference
// view plus one local, session-guarded update route when mounted by reviewd.
func NewNotificationPreferencesHandler(options NotificationPreferencesOptions) http.Handler {
	mux := http.NewServeMux()
	handler := notificationPreferencesHandler{store: options.Store, now: options.Now}
	mux.HandleFunc("GET "+notificationPreferencesPath, handler.get)
	mux.HandleFunc("POST /api/v1/mutate/notification-preferences", handler.update)
	return mux
}

func registerNotificationPreferenceRoutes(mux *http.ServeMux, options NotificationPreferencesOptions) {
	if mux == nil {
		return
	}
	handler := notificationPreferencesHandler{store: options.Store, now: options.Now}
	mux.HandleFunc("GET "+notificationPreferencesPath, handler.get)
	mux.HandleFunc("POST /api/v1/mutate/notification-preferences", handler.update)
}

type notificationPreferencesHandler struct {
	store NotificationPreferencesStore
	now   func() time.Time
}

type notificationChannelsResponse struct {
	Browser bool `json:"browser"`
	Log     bool `json:"log"`
	Sound   bool `json:"sound"`
	TTS     bool `json:"tts"`
}

// notificationPreferencesResponse excludes opaque template settings and
// machine-local sound paths, which can contain private or future-sensitive
// data. Only safe, defined local controls cross this API boundary.
type notificationPreferencesResponse struct {
	Version         int64                        `json:"version"`
	Channels        notificationChannelsResponse `json:"channels"`
	MutedUntil      *time.Time                   `json:"muted_until"`
	SpeechRateMilli int                          `json:"speech_rate_milli"`
	UpdatedAt       time.Time                    `json:"updated_at"`
}

type notificationPreferencesUpdateRequest struct {
	ExpectedVersion *int64          `json:"expected_version"`
	Channels        json.RawMessage `json:"channels"`
	MutedUntil      json.RawMessage `json:"muted_until"`
	SpeechRateMilli *int            `json:"speech_rate_milli"`
}

func (h notificationPreferencesHandler) get(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "notification preferences are available only on loopback", false)
		return
	}
	if request.URL.RawQuery != "" {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "notification preferences do not accept query parameters", false)
		return
	}
	preferences, ok := h.load(response, request)
	if !ok {
		return
	}
	result, err := safeNotificationPreferencesResponse(preferences)
	if err != nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_preferences_unavailable", "notification preferences are unavailable", true)
		return
	}
	writeControlJSON(response, http.StatusOK, result)
}

func (h notificationPreferencesHandler) update(response http.ResponseWriter, request *http.Request) {
	var input notificationPreferencesUpdateRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if input.ExpectedVersion == nil || input.SpeechRateMilli == nil || input.Channels == nil || input.MutedUntil == nil {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "notification preference fields are required", false)
		return
	}
	channels, err := parseNotificationChannels(input.Channels, false)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "notification channels are invalid", false)
		return
	}
	mutedUntil, err := parseNotificationMuteTime(input.MutedUntil)
	if err != nil || *input.ExpectedVersion < 1 || *input.SpeechRateMilli < 500 || *input.SpeechRateMilli > 2000 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "notification preference fields are invalid", false)
		return
	}
	current, ok := h.load(response, request)
	if !ok {
		return
	}
	mergedChannels, err := mergeNotificationChannels(current.ChannelsJSON, channels)
	if err != nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_preferences_unavailable", "notification preferences are unavailable", true)
		return
	}
	updated, err := h.store.UpdateNotificationPreferences(request.Context(), sqlite.UpdateNotificationPreferencesInput{
		ExpectedVersion: *input.ExpectedVersion, ChannelsJSON: mergedChannels, QuietHoursJSON: current.QuietHoursJSON,
		EventTemplatesJSON: current.EventTemplatesJSON, MutedUntil: mutedUntil, SpeechRateMilli: *input.SpeechRateMilli,
		CustomSoundPath: current.CustomSoundPath, UpdatedAt: h.currentTime(),
	})
	if err != nil {
		if errors.Is(err, sqlite.ErrNotificationPreferencesConflict) {
			writeControlError(response, http.StatusConflict, "notification_preferences_conflict", "notification preferences changed; reload before saving", false)
			return
		}
		writeControlError(response, http.StatusInternalServerError, "mutation_failed", "could not update notification preferences", true)
		return
	}
	result, err := safeNotificationPreferencesResponse(updated)
	if err != nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_preferences_unavailable", "notification preferences are unavailable", true)
		return
	}
	writeControlJSON(response, http.StatusOK, result)
}

func (h notificationPreferencesHandler) load(response http.ResponseWriter, request *http.Request) (sqlite.NotificationPreferences, bool) {
	if h.store == nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_preferences_unavailable", "notification preferences are unavailable", true)
		return sqlite.NotificationPreferences{}, false
	}
	preferences, err := h.store.LoadNotificationPreferences(request.Context())
	if err != nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_preferences_unavailable", "notification preferences are unavailable", true)
		return sqlite.NotificationPreferences{}, false
	}
	return preferences, true
}

func (h notificationPreferencesHandler) currentTime() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

func safeNotificationPreferencesResponse(preferences sqlite.NotificationPreferences) (notificationPreferencesResponse, error) {
	channels, err := parseNotificationChannels(preferences.ChannelsJSON, true)
	if err != nil || preferences.Version < 1 || preferences.SpeechRateMilli < 500 || preferences.SpeechRateMilli > 2000 {
		return notificationPreferencesResponse{}, errors.New("stored notification preferences are invalid")
	}
	return notificationPreferencesResponse{
		Version: preferences.Version, Channels: channels, MutedUntil: preferences.MutedUntil,
		SpeechRateMilli: preferences.SpeechRateMilli, UpdatedAt: preferences.UpdatedAt,
	}, nil
}

func parseNotificationChannels(raw json.RawMessage, allowUnknown bool) (notificationChannelsResponse, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return notificationChannelsResponse{}, errors.New("channels are not an object")
	}
	if !allowUnknown {
		for key := range values {
			switch key {
			case "browser", "log", "sound", "tts":
			default:
				return notificationChannelsResponse{}, errors.New("channel is unknown")
			}
		}
	}
	decode := func(key string) (bool, error) {
		value, found := values[key]
		if !found {
			return false, errors.New("channel is absent")
		}
		var enabled bool
		if err := json.Unmarshal(value, &enabled); err != nil {
			return false, err
		}
		return enabled, nil
	}
	browser, err := decode("browser")
	if err != nil {
		return notificationChannelsResponse{}, err
	}
	log, err := decode("log")
	if err != nil {
		return notificationChannelsResponse{}, err
	}
	sound, err := decode("sound")
	if err != nil {
		return notificationChannelsResponse{}, err
	}
	tts, err := decode("tts")
	if err != nil {
		return notificationChannelsResponse{}, err
	}
	return notificationChannelsResponse{Browser: browser, Log: log, Sound: sound, TTS: tts}, nil
}

func parseNotificationMuteTime(raw json.RawMessage) (*time.Time, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var mutedUntil time.Time
	if err := json.Unmarshal(raw, &mutedUntil); err != nil || mutedUntil.IsZero() || mutedUntil.UnixMicro() < 0 {
		return nil, errors.New("mute time is invalid")
	}
	mutedUntil = mutedUntil.UTC()
	return &mutedUntil, nil
}

func mergeNotificationChannels(current json.RawMessage, next notificationChannelsResponse) ([]byte, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(current, &values); err != nil || values == nil {
		return nil, errors.New("stored channels are invalid")
	}
	for key, enabled := range map[string]bool{
		"browser": next.Browser, "log": next.Log, "sound": next.Sound, "tts": next.TTS,
	} {
		value, err := json.Marshal(enabled)
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	encoded, err := json.Marshal(values)
	if err != nil || len(encoded) == 0 || len(encoded) > 64*1024 {
		return nil, errors.New("stored channels are invalid")
	}
	return encoded, nil
}
