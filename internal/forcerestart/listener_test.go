package forcerestart_test

import (
	"testing"

	"github.com/glenjbarber/apiary/internal/forcerestart"
)

// ListeningFrom is the only confirmation force-restart has, so these
// cases are about what it refuses to accept as a listener. Every
// negative here is a row that a looser check would have been satisfied
// by, and on a live Comb being satisfied by one of them means a
// half-restarted pair reported as a clean run.
//
// The positive rows are the ordinary shapes sockstat(8) prints on
// FreeBSD, including the wildcard bind, because managerd and raftd both
// bind 127.0.0.1 in the default configuration and a checker that only
// understood one of them would fail closed on a correct host.

func TestListeningFrom_AcceptsRealListenerRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		port   int
	}{
		{
			name: "loopback bind, the default for both daemons",
			port: 17700,
			output: `USER     COMMAND    PID   FD PROTO  LOCAL ADDRESS         FOREIGN ADDRESS
root     managerd   4242  7  tcp4   127.0.0.1:17700      *:*
`,
		},
		{
			name:   "wildcard bind",
			port:   17700,
			output: "root     managerd   4242  7  tcp4   *:17700              *:*\n",
		},
		{
			name:   "a routable bind",
			port:   17600,
			output: "root     raftd      4243  7  tcp4   192.0.2.10:17600     *:*\n",
		},
		{
			name: "one match among several rows, on a later line",
			port: 17600,
			output: `USER     COMMAND    PID   FD PROTO  LOCAL ADDRESS         FOREIGN ADDRESS
root     managerd   4242  6  tcp4   127.0.0.1:17700      *:*
root     raftd      4243  7  tcp4   127.0.0.1:17600      *:*
root     restshimd  4244  7  tcp4   127.0.0.1:8081       *:*
`,
		},
		{
			name:   "no trailing newline, which is what a truncated read gives",
			port:   17700,
			output: "root     managerd   4242  7  tcp4   127.0.0.1:17700      *:*",
		},
		{
			name:   "an IPv6-mapped listener on the same port, -4 notwithstanding",
			port:   17700,
			output: "root     managerd   4242  7  tcp6   [::1]:17700           *:*\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !forcerestart.ListeningFrom(tc.output, tc.port) {
				t.Errorf("a real listener on port %d was not recognised:\n%s", tc.port, tc.output)
			}
		})
	}
}

func TestListeningFrom_RejectsEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{
			// The case a "simplify the pattern" edit breaks silently:
			// the digits are exactly right and only the delimiter is
			// missing, so it reads correctly in a diff.
			name:   "the port's digits with the host: delimiter dropped",
			output: "root    odd      4242  4  tcp4  127.0.0.1.17700    *:*\n",
		},
		{
			name:   "a bare port number with no host at all",
			output: "17700\n",
		},
		{
			// Raftd's own port. A check that confirmed "an apiary
			// socket" rather than this service's socket passes here.
			name:   "the other daemon's port",
			output: "root    raftd     4243  4  tcp4  127.0.0.1:17600    *:*\n",
		},
		{
			name:   "a port with managerd's digits as a prefix",
			output: "root    odd      4242  4  tcp4  127.0.0.1:177000   *:*\n",
		},
		{
			name:   "a port with managerd's digits as a suffix",
			output: "root    odd      4242  4  tcp4  127.0.0.1:117700   *:*\n",
		},
		{
			name:   "managerd's digits inside an unrelated number",
			output: "root    odd      4242  4  tcp4  127.0.0.1:17701    *:*\n",
		},
		{
			name:   "the digits inside a connection string, not a local bind",
			output: "root    odd      4242  4  tcp4  127.0.0.1:17700->192.0.2.9:5000  *:*\n",
		},
		{
			name:   "the column header, which is a row of fields like any other",
			output: "USER COMMAND PID FD PROTO LOCAL ADDRESS FOREIGN ADDRESS\n",
		},
		{
			name:   "nothing listening at all",
			output: "USER COMMAND PID FD PROTO LOCAL ADDRESS FOREIGN ADDRESS\n",
		},
		{
			// The failure mode that matters most: if sockstat's layout
			// ever changes, this must return false and time out loudly,
			// never true and pass quietly.
			name:   "output whose layout has nothing recognisable in it",
			output: "managerd is up\nraftd is up\n",
		},
		{
			name:   "empty output",
			output: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if forcerestart.ListeningFrom(tc.output, 17700) {
				t.Errorf("port 17700 was reported listening by a row that is not one:\n%s", tc.output)
			}
		})
	}
}

func TestListeningFrom_RefusesANonsensicalPort(t *testing.T) {
	// A port that is not a port must not match a row, and must not
	// panic either: the plan's own preflight is what is supposed to
	// catch this, and a check that crashed would be a worse outcome
	// than a check that declined.
	row := "root    managerd  4242  7  tcp4  127.0.0.1:17700    *:*\n"
	for _, port := range []int{0, -1, -17700} {
		if forcerestart.ListeningFrom(row, port) {
			t.Errorf("port %d was reported listening", port)
		}
	}
}
