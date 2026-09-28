package frontend

import (
	"strings"
	"testing"
)

// TestJoinerLogStateText_NeverRendersUnobservedAsSafe is the UI half of
// the fail-closed rule. An Admin reading "safe to approve" next to a
// request whose evidence is simply absent would be actively misled, and
// the misleading would happen in the one place - the pending list - where
// the operator looks BEFORE acting. All three shapes are distinct, and the
// absent one has to name the consequence rather than look benign.
func TestJoinerLogStateText_NeverRendersUnobservedAsSafe(t *testing.T) {
	empty := joinerLogStateText(true, 0)
	if !strings.Contains(empty, "safe to approve") {
		t.Errorf("observed empty = %q, want it to say the request is safe to approve", empty)
	}
	if strings.Contains(empty, "refused") {
		t.Errorf("observed empty = %q, want no refusal language", empty)
	}

	notEmpty := joinerLogStateText(true, 4096)
	if !strings.Contains(notEmpty, "4096") {
		t.Errorf("observed non-empty = %q, want it to name the index", notEmpty)
	}
	if !strings.Contains(notEmpty, "cannot be approved") {
		t.Errorf("observed non-empty = %q, want it to say approval is impossible", notEmpty)
	}

	unobserved := joinerLogStateText(false, 0)
	if !strings.Contains(unobserved, "not reported") {
		t.Errorf("unobserved = %q, want it to say the evidence is absent", unobserved)
	}
	if !strings.Contains(unobserved, "refused") {
		t.Errorf("unobserved = %q, want it to say approval will be refused - silence here reads as safety", unobserved)
	}
	// The failure mode this test exists for: an absent reading rendering
	// with the same words as a safe one.
	if unobserved == empty {
		t.Errorf("unobserved and observed-empty render identically (%q), so an absent reading looks safe", empty)
	}
}
