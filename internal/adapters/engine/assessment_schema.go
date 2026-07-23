package engine

import (
	"fmt"
	"os"
)

// WriteAssessmentSchema writes the native CLI structured-output boundary. The
// assessment package remains final validator for evidence anchors and enums.
func WriteAssessmentSchema(path string) error {
	const schema = `{"type":"object","additionalProperties":false,"required":["version","verdict","summary","confidence","limitations","coverage","findings"],"properties":{"version":{"const":1},"verdict":{"type":"string"},"summary":{"type":"string"},"confidence":{"type":"string"},"limitations":{"type":"array","items":{"type":"string"}},"coverage":{"type":"object"},"findings":{"type":"array"}}}` + "\n"
	if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
		return fmt.Errorf("write assessment schema: %w", err)
	}
	return nil
}
