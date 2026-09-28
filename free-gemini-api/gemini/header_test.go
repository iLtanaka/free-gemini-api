package gemini

import (
	"encoding/json"
	"testing"
	"time"
)

// TestFlashHeaderValueShape checks the x-goog-ext-525001261-jspb payload
// against the structure observed in a real Chrome DevTools capture of a
// StreamGenerate request - see the fix that replaced the old truncated
// [4]-only capability array and the missing session-identity tail.
func TestFlashHeaderValueShape(t *testing.T) {
	c := &GeminiClient{sessionUUID: "test-uuid-1234", sessionStart: time.Now().Add(-5 * time.Second)}

	var got []interface{}
	if err := json.Unmarshal([]byte(c.flashHeaderValue()), &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("expected 20 top-level elements (matching real capture), got %d: %v", len(got), got)
	}
	if got[4] != flashModelID {
		t.Errorf("index 4 (model id): got %v, want %v", got[4], flashModelID)
	}
	flags, ok := got[8].([]interface{})
	if !ok || len(flags) != 10 {
		t.Errorf("index 8 (capability flags): expected a 10-element array, got %v", got[8])
	}
	if got[16] != "test-uuid-1234" {
		t.Errorf("index 16 (session uuid): got %v, want test-uuid-1234", got[16])
	}
	tail, ok := got[19].([]interface{})
	if !ok || len(tail) != 2 {
		t.Errorf("index 19 (timing tail): expected a 2-element array, got %v", got[19])
	}
}

func TestRequestSessionHeaderValueIsRealUUID(t *testing.T) {
	c := &GeminiClient{requestUUID: "B856FD54-0579-495E-9692-412CAD32F0F1"}
	got := c.requestSessionHeaderValue()
	want := `["B856FD54-0579-495E-9692-412CAD32F0F1",1]`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
