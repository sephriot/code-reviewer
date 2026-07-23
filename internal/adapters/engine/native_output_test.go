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
