package audit

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestLog(t *testing.T) {
	var b bytes.Buffer
	l := New(&b)
	l.Log(Record{Time: time.Unix(0, 0).UTC(), Method: "session/new", User: "u", Outcome: OK})
	var r Record
	if err := json.Unmarshal(b.Bytes(), &r); err != nil || r.Method != "session/new" || r.Outcome != OK {
		t.Fatal(b.String(), err)
	}
	var nl *Logger
	nl.Log(Record{})
}
