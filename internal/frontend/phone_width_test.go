package frontend

import (
	"regexp"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/web"
)

// TestPhoneWidthTopBarActionsDoNotOverflow guards the phone-width
// layout rule the UI redesign handoff states in section 12: "The page
// itself never scrolls horizontally."
//
// The top bar's actions row carries the session's identity and the
// mutating actions - "Add comb", "Create cell", "Log out". It was
// declared flex:none with nowrap children, so at 390px the row was
// wider than the viewport, the "Log out" button was cut off at the
// right edge, and the page scrolled sideways.
//
// The fix lets the row wrap and lets the account label shrink. This
// test pins both, because a CSS rule that stops being applied is
// invisible in every other test in this package: the markup is
// correct and the BEHAVIOUR is wrong, which is the same shape as the
// three refresh-path bugs the overview found.
//
// A test cannot measure a browser's layout, so this asserts on the
// rules themselves rather than on a rendered result - the same
// approach the other layout.html assertions in this package take.
func TestPhoneWidthTopBarActionsDoNotOverflow(t *testing.T) {
	layout, err := web.FS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	css := string(layout)

	// The phone-width block is the one under the 620px media query.
	idx := strings.Index(css, "@media (max-width:620px)")
	if idx < 0 {
		t.Fatal("layout.html has no 620px media query; the phone-width rules are gone")
	}
	phone := css[idx:]

	// The actions row must be allowed to wrap and to take a full row
	// of its own, and must not be pinned to a non-shrinking width.
	if !strings.Contains(phone, ".topbar-actions") {
		t.Fatal("the 620px media query no longer styles .topbar-actions")
	}
	block := phone[strings.Index(phone, ".topbar-actions"):]
	if end := strings.Index(block, "}"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{"flex-wrap:wrap", "min-width:0"} {
		if !strings.Contains(block, want) {
			t.Errorf("the phone-width .topbar-actions rule has no %q; the actions row can overflow the viewport again", want)
		}
	}

	// The base rule's flex:none is what caused the overflow, so the
	// phone-width rule has to override it. A rule that only adds
	// properties without resetting flex:none would leave the row
	// unshrinkable.
	if strings.Contains(block, "flex:none") {
		t.Error("the phone-width .topbar-actions rule keeps flex:none, which is what made the row overflow")
	}

	// The base rule must still say flex:none - that is correct on a
	// wide screen, where the row should not stretch. The phone-width
	// rule is what overrides it. If the base rule ever loses flex:none,
	// the base rule's intent changed and this test should say so.
	base := css[:idx]
	baseBlock := base[strings.LastIndex(base, ".topbar-actions"):]
	if end := strings.Index(baseBlock, "}"); end > 0 {
		baseBlock = baseBlock[:end]
	}
	if !regexp.MustCompile(`flex\s*:\s*none`).MatchString(baseBlock) {
		t.Error("the base .topbar-actions rule no longer says flex:none; if that was changed on purpose, this test and the phone-width rule need to agree")
	}
}
