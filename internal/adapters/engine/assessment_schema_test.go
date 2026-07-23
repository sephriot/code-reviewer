package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAssessmentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assessment-schema.json")
	if err := WriteAssessmentSchema(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value["type"] != "object" || value["additionalProperties"] != false {
		t.Fatalf("schema=%v", value)
	}
}
