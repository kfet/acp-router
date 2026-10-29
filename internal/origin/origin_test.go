package origin

import (
	"errors"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	o := Origin{Platform: "slack", TeamID: "T1", UserID: "U1", ChannelID: "C1", ThreadID: "1.2", IsDM: true}
	// Simulate a JSON-decoded _meta.
	m := map[string]any{Key: map[string]any{"platform": "slack", "team_id": "T1", "user_id": "U1", "channel_id": "C1", "thread_id": "1.2", "is_dm": true}}
	got, err := FromMeta(m)
	if err != nil || got != o {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = FromMeta(o.Meta())
	if err != nil || got != o {
		t.Fatalf("Meta round trip: %+v, %v", got, err)
	}
}

func TestErrors(t *testing.T) {
	if _, err := FromMeta(nil); !errors.Is(err, ErrMissing) {
		t.Fatalf("nil: %v", err)
	}
	if _, err := FromMeta(map[string]any{Key: map[string]any{"platform": "x"}}); err == nil {
		t.Fatal("missing user_id accepted")
	}
	if _, err := FromMeta(map[string]any{Key: "str"}); err == nil {
		t.Fatal("string accepted")
	}
	if _, err := FromMeta(map[string]any{Key: func() {}}); err == nil {
		t.Fatal("unmarshalable accepted")
	}
}
