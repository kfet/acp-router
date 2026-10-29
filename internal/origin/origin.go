// Package origin is a vendored copy of the acp-kit origin _meta contract
// (github.com/kfet/acp-kit/origin). Relays attach it to session/new,
// session/load, session/fork and every session/prompt so an agent-side
// component knows who is speaking without parsing prompt text.
//
// TODO(acp-kit): replace this package with github.com/kfet/acp-kit/origin
// once it is released; the key, field names and JSON shape are identical.
package origin

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Key is the _meta key carrying the Origin object.
const Key = "dev.acp-kit.origin/v1"

// Origin identifies where a request came from on the relay's platform.
type Origin struct {
	Platform  string `json:"platform"`
	TeamID    string `json:"team_id,omitempty"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	IsDM      bool   `json:"is_dm,omitempty"`
	IsMention bool   `json:"is_mention,omitempty"`
}

// ErrMissing reports that a _meta map carried no origin entry.
var ErrMissing = errors.New("origin: _meta has no " + Key)

// Meta returns a _meta map holding o under Key.
func (o Origin) Meta() map[string]any { return map[string]any{Key: o} }

// Validate reports whether o carries the fields every consumer relies on.
func (o Origin) Validate() error {
	if o.Platform == "" || o.UserID == "" {
		return fmt.Errorf("origin: platform and user_id are required")
	}
	return nil
}

// FromMeta extracts and validates the Origin in a decoded _meta value.
// meta may be a map[string]any (as decoded by encoding/json) or nil.
func FromMeta(meta any) (Origin, error) {
	m, _ := meta.(map[string]any)
	v, ok := m[Key]
	if !ok {
		return Origin{}, ErrMissing
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return Origin{}, fmt.Errorf("origin: %w", err)
	}
	var o Origin
	if err := json.Unmarshal(raw, &o); err != nil {
		return Origin{}, fmt.Errorf("origin: %w", err)
	}
	return o, o.Validate()
}
