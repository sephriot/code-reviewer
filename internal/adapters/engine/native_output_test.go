package engine

import "testing"

func TestNormalizeNativeOutput(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"version":1}`), []byte(`{"result":"{\"version\":1}"}`), []byte(`{"message":"{\"version\":1}"}`)} {
		got, err := NormalizeNativeOutput(ProviderClaude, raw)
		if err != nil || string(got) != `{"version":1}` {
			t.Fatalf("got=%s err=%v", got, err)
		}
	}
	if _, err := NormalizeNativeOutput(ProviderCodex, []byte(`{"result":"not json"}`)); err == nil {
		t.Fatal("prose accepted")
	}
}

func TestNormalizeNativeOutputAcceptsOnlyFencedJSONDocument(t *testing.T) {
	got, err := NormalizeNativeOutput(ProviderAgent, []byte("{\"result\":\"```json\\n{\\\"version\\\":1}\\n```\"}"))
	if err != nil || string(got) != `{"version":1}` {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := NormalizeNativeOutput(ProviderAgent, []byte(`{"result":"prefix {\"version\":1}"}`)); err == nil {
		t.Fatal("prose-wrapped JSON accepted")
	}
}
