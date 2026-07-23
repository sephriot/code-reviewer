package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
	return nil, fmt.Errorf("native engine output has no assessment JSON document: %s", nativeOutputShape(direct))
}

func nativeOutputShape(value map[string]json.RawMessage) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	details := make([]string, 0, len(keys))
	for _, key := range keys {
		field := value[key]
		var text string
		if json.Unmarshal(field, &text) == nil {
			details = append(details, key+"=string("+fmt.Sprintf("%d", len(text))+")")
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(field, &object) == nil {
			details = append(details, key+"=object")
			continue
		}
		details = append(details, key+"=non-string")
	}
	return "fields=" + fmt.Sprintf("%v", details)
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
		return embeddedJSONObject(candidate)
	}
	return candidate, true
}

// embeddedJSONObject tolerates provider framing prose while returning only a
// complete JSON object. The assessment validator remains the authority for
// every accepted field, enum, and evidence anchor.
func embeddedJSONObject(raw []byte) ([]byte, bool) {
	for start, value := range raw {
		if value != '{' {
			continue
		}
		depth := 0
		quoted := false
		escaped := false
		for end := start; end < len(raw); end++ {
			character := raw[end]
			if quoted {
				if escaped {
					escaped = false
				} else if character == '\\' {
					escaped = true
				} else if character == '"' {
					quoted = false
				}
				continue
			}
			switch character {
			case '"':
				quoted = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					candidate := raw[start : end+1]
					if json.Valid(candidate) {
						return candidate, true
					}
					break
				}
			}
		}
	}
	return nil, false
}
