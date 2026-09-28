package forcerestart

import (
	"net"
	"strconv"
	"strings"
)

// ListeningFrom reports whether sockstat's output shows a listening
// socket on port. The input is expected to be what `sockstat -4 -l`
// prints; anything that is not parseable is treated as "not
// listening".
//
// # WHY THIS PARSES INSTEAD OF MATCHING A SUBSTRING
//
// The confirmation this feeds is the only one force-restart has, so it
// has to be the one measurement that cannot be fooled. A check that
// looked for the digits 17700 anywhere in the output would be
// satisfied by a foreign port ending in those digits, by a row for
// some unrelated daemon, and by the words of a column header. A check
// that looked for ":17700" would still be satisfied by 127.0.0.1:177000
// or by an address that merely contains it. So a field only counts if
// it is a complete host:port pair whose port is exactly the one asked
// about.
//
// It deliberately scans every whitespace-delimited field on every line
// rather than counting to a fixed column. Column positions are a
// property of sockstat's printf format, and pinning them would trade
// one silent failure for another: a layout change would make the
// columns shift and the real listener stop being found. Scanning fields
// depends only on addresses being whitespace-delimited, which is what
// sockstat has always done, and a layout change that broke even that
// fails closed - no field parses, so nothing is ever confirmed and the
// wait times out loudly rather than passing on a false reading.
func ListeningFrom(sockstatOutput string, port int) bool {
	if port <= 0 {
		return false
	}
	want := strconv.Itoa(port)
	for _, field := range strings.Fields(sockstatOutput) {
		// A field that is not a host:port pair - a bare port number, a
		// hostname, a column header, a decoy row with the delimiter
		// dropped - is not evidence of a listener. SplitHostPort
		// requires the colon, and returns an error without one.
		host, got, err := net.SplitHostPort(field)
		if err != nil || host == "" {
			continue
		}
		if got == want {
			return true
		}
	}
	return false
}
