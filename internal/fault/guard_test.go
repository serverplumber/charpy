package fault

import (
	"strings"
	"testing"

	"github.com/serverplumber/charpy/internal/envelope"
	"github.com/serverplumber/charpy/internal/interpose"
)

// The guard is white-box on purpose: it exists to catch a mechanism that
// forgot to say what becomes of the matched frame, and a mechanism is
// unexported. Reaching it through Apply would only prove the eight shipped
// today are right, not that the ninth cannot be wrong.
func TestPlannedRefusesAFrameWithNoFate(t *testing.T) {
	kind := interpose.Case{Fault: interpose.Fault{Kind: "invented"}}
	m, err := envelope.NewResponse(envelope.NumberID(1), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("neither delivered, held nor swallowed", func(t *testing.T) {
		_, err := planned(Plan{Case: kind, Before: []envelope.Message{m}}, nil)
		if err == nil {
			t.Fatal("a plan that drops the matched frame was accepted")
		}
		if !strings.Contains(err.Error(), "invented") {
			t.Errorf("err = %v, want the mechanism named", err)
		}
	})

	t.Run("each of the three answers is enough", func(t *testing.T) {
		for name, p := range map[string]Plan{
			"deliver": {Case: kind, Deliver: &m},
			"hold":    {Case: kind, Hold: &Hold{}},
			"swallow": {Case: kind, Swallow: true},
			"synthesize and deliver": {Case: kind,
				Before: []envelope.Message{m}, Deliver: &m},
		} {
			if _, err := planned(p, nil); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}
