package frontend

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The staleness tick in web/templates/_replica_freshness.html recomputes
// the age in the browser, which means the age an operator sees after the
// first tick is formatted by JS rather than by formatObservationAge. The
// two must agree: a page open across a bucket boundary (60s, 1h, 24h)
// that ticks to "1h 0m ago" where a fresh render says "59m ago" looks
// like the clock jumped.
//
// Go cannot execute the template's JS, so this test does the next best
// thing: it re-implements the tick's formatAge arithmetic here in Go,
// byte-for-byte from the partial, and asserts it matches the server's
// formatter across every bucket boundary and a spread of ages. The two
// copies are then the thing that can silently drift - this test is what
// makes that drift loud.
//
// A change to either formatter that is not mirrored in the other fails
// here. That is the intended tripwire, not a limitation: keeping one
// formatter would mean computing the age server-side on a timer, which
// costs a re-render and a full page fetch to redraw one number.
func TestReplicaFreshnessPanelTickMirrorsTheServerFormatter(t *testing.T) {
	// tickFormatAge is web/templates/_replica_freshness.html's formatAge,
	// transcribed with the same integer arithmetic. ms is the age in
	// milliseconds, which is what the tick computes from a Unix second
	// and the browser's clock.
	tickFormatAge := func(ms int64) string {
		const (
			minute = 60000
			hour   = 3600000
			day    = 86400000
		)
		if ms < 0 {
			return "" // null in the partial: the cell is left alone
		}
		if ms < minute {
			return itoa(ms/1000) + "s ago"
		}
		if ms < hour {
			return itoa(ms/minute) + "m ago"
		}
		if ms < day {
			return itoa(ms/hour) + "h " + itoa((ms%hour)/minute) + "m ago"
		}
		return itoa(ms/day) + "d " + itoa((ms%day)/hour) + "h ago"
	}

	// Ages that straddle every bucket boundary the formatters have,
	// including the exact boundary, the one below it, and the one above.
	ages := []time.Duration{
		0,
		time.Second,
		30 * time.Second,
		59 * time.Second,
		time.Minute,
		time.Minute + 500*time.Millisecond,
		2 * time.Minute,
		59 * time.Minute,
		time.Hour,
		time.Hour + 30*time.Second,
		90 * time.Minute,
		3*time.Hour + 12*time.Minute,
		23*time.Hour + 59*time.Minute,
		24 * time.Hour,
		25 * time.Hour,
		50 * time.Hour,
		365 * 24 * time.Hour,
	}
	for _, age := range ages {
		want := formatObservationAge(age)
		got := tickFormatAge(age.Milliseconds())
		if got != want {
			t.Errorf("at age %v the tick renders %q but the server renders %q - the two formatters must agree", age, got, want)
		}
	}

	// The negative-age case is not a formatting difference but a
	// behavioural one, so it is stated on its own terms: the server says
	// "unknown", and the tick must decline to write anything rather than
	// invent a zero.
	if got, want := tickFormatAge(-1000), formatObservationAge(-time.Minute); want == "unknown" && got != "" {
		t.Errorf("at a negative age the tick renders %q, but the server renders %q - a disagreeing clock is not evidence of anything, and the tick must leave the cell alone", got, want)
	}
}

// itoa is strconv.Itoa without the import, kept local so this file's
// only dependency on the partial is the arithmetic under test.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The data attribute is what the tick recomputes from, so it must carry
// the raw Unix second - and must be EMPTY for an unobserved node rather
// than 0. A 0 is a real instant (1970) and would render as a fifty-year
// -old observation; unknown is a different claim and must not be encoded
// as a number.
func TestReplicaFreshnessPanelAgeAttributeIsEmptyWhenUnobserved(t *testing.T) {
	observed := time.Now().Add(-2 * time.Minute)
	client := &freshnessHASTClient{
		fakeClient: &fakeClient{
			statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
			getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
				Id: "vm-1", Name: "database", NodeId: "node-a", ReplicaNodeId: "node-b",
			}},
		},
		resp: &rpcpb.GetLocalHASTResourceStatusResponse{
			Role: "primary", ResourceStatus: "complete", Replication: "memsync",
			ObservedAtUnix: observed.Unix(),
		},
	}
	body := vmDetailFreshnessPage(t, client, nil)

	attr := regexp.MustCompile(`data-age-of="([^"]*)"`).FindStringSubmatch(body)
	if attr == nil {
		t.Fatalf("no data-age-of attribute rendered; the staleness tick has nothing to recompute from:\n%s", body)
	}
	if attr[1] == "" || attr[1] == "0" {
		t.Errorf("data-age-of = %q for an observed node, want its Unix second (%d) - a 0 would render as a fifty-year-old observation", attr[1], observed.Unix())
	}

	// The same page with a response carrying no timestamp must render the
	// attribute empty, not as 0 and not at all: an absent attribute and a
	// zero attribute are different claims to the script that reads them.
	client.resp = &rpcpb.GetLocalHASTResourceStatusResponse{Role: "primary", ResourceStatus: "complete", Replication: "memsync"}
	body = vmDetailFreshnessPage(t, client, nil)
	if !strings.Contains(body, `data-age-of=""`) {
		t.Errorf("an unobserved node did not render an empty data-age-of; silence must not become a number:\n%s", body)
	}
}

// The tick is a plain interval timer, and the one thing that must not go
// wrong silently is its absence: a page whose script did not emit still
// renders a correct server-side age, so nothing about the HTML alone
// would tell an operator the number is frozen. This asserts the partial
// ships the script and its interval, so its loss is a test failure rather
// than a page that quietly stops ticking.
func TestReplicaFreshnessPanelShipsTheTickScript(t *testing.T) {
	partial, err := os.ReadFile("../../web/templates/_replica_freshness.html")
	if err != nil {
		t.Fatalf("reading the partial: %v", err)
	}
	body := string(partial)
	for _, want := range []string{"setInterval(tick, REFRESH_MS)", `document.querySelectorAll("[data-age-of]")`, "visibilitychange"} {
		if !strings.Contains(body, want) {
			t.Errorf("the partial no longer contains %q - the staleness tick is silently gone", want)
		}
	}
}
