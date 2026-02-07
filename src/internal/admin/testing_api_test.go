package admin

import "testing"

func TestParseModelIDsFromDataList(t *testing.T) {
	raw := []byte(`{"object":"list","data":[{"id":"gpt-4o"},{"id":"claude-3-5-sonnet-latest"},{"id":""},{"foo":"bar"}]}`)
	ids := parseModelIDsFromDataList(raw)
	if len(ids) != 2 {
		t.Fatalf("expected 2 ids, got %d: %#v", len(ids), ids)
	}
	if ids[0] != "gpt-4o" || ids[1] != "claude-3-5-sonnet-latest" {
		t.Fatalf("unexpected ids: %#v", ids)
	}
}

