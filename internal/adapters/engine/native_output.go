package engine

import (
	"bytes"
	"encoding/json"
	"errors"
)

// NormalizeNativeOutput extracts one assessment document from a provider's
// final JSON envelope. It never accepts prose or partial stream events.
func NormalizeNativeOutput(provider Provider, raw []byte) ([]byte, error) {
	if provider != ProviderClaude && provider != ProviderCodex && provider != ProviderAgent {
		return nil, errors.New("native engine provider is invalid")
	}
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return nil, errors.New("native engine output is not JSON")
	}
	var direct map[string]json.RawMessage
	if err := json.Unmarshal(raw, &direct); err != nil {
		return nil, err
	}
	if _, ok := direct["version"]; ok {
		return raw, nil
	}
	for _, key := range []string{"result", "output", "text", "message"} {
		var value string
		if field, ok := direct[key]; ok && json.Unmarshal(field, &value) == nil {
			if candidate, valid := assessmentDocument([]byte(value)); valid {
				return candidate, nil
			}
		}
	}
	return nil, errors.New("native engine output has no assessment JSON document")
}

func assessmentDocument(raw []byte) ([]byte, bool) {
	candidate := bytes.TrimSpace(raw)
	if bytes.HasPrefix(candidate, []byte("```")) {
		newline := bytes.IndexByte(candidate, '\n')
		if newline < 0 || !bytes.HasSuffix(candidate, []byte("```")) {
			return nil, false
		}
		language := bytes.TrimSpace(candidate[3:newline])
		if len(language) != 0 && !bytes.Equal(language, []byte("json")) {
			return nil, false
		}
		candidate = bytes.TrimSpace(candidate[newline+1 : len(candidate)-3])
	}
	if !json.Valid(candidate) {
		return nil, false
	}
	return candidate, true
}
