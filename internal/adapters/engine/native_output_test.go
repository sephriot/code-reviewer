package engine

import (
	"strings"
	"testing"
)

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

func TestNormalizeNativeOutputExtractsProviderFraming(t *testing.T) {
	got, err := NormalizeNativeOutput(ProviderAgent, []byte("{\"result\":\"```json\\n{\\\"version\\\":1}\\n```\"}"))
	if err != nil || string(got) != `{"version":1}` {
		t.Fatalf("got=%q err=%v", got, err)
	}
	got, err = NormalizeNativeOutput(ProviderAgent, []byte(`{"result":"Here is result: {\"version\":1}"}`))
	if err != nil || string(got) != `{"version":1}` {
		t.Fatalf("prose got=%q err=%v", got, err)
	}
	if _, err := NormalizeNativeOutput(ProviderAgent, []byte(`{"result":"prefix {bad}"}`)); err == nil {
		t.Fatal("invalid embedded JSON accepted")
	} else if !strings.Contains(err.Error(), "fields=[result=string(12)]") {
		t.Fatalf("missing safe shape: %v", err)
	}
}
