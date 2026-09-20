package frontend

import "testing"

func TestHostOnly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"10.62.0.5", "10.62.0.5"},
		{"10.62.0.5:17600", "10.62.0.5"},
		{"0.0.0.0:8080", "0.0.0.0"},
	}
	for _, c := range cases {
		if got := hostOnly(c.in); got != c.want {
			t.Errorf("hostOnly(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWithFixedPort_IgnoresAnyPortInHost is ADR's own regression test:
// this project's own fixed-port fields must never let a caller choose
// a different port, even one smuggled into the "host" value itself
// (e.g. a value pasted from a full address elsewhere, or a request
// crafted outside the browser) - the fixed port always wins.
func TestWithFixedPort_IgnoresAnyPortInHost(t *testing.T) {
	cases := []struct{ host, port, want string }{
		{"10.62.0.5", "17700", "10.62.0.5:17700"},
		{"10.62.0.5:9999", "17700", "10.62.0.5:17700"},
		{" 10.62.0.5 ", "17700", "10.62.0.5:17700"},
		{"", "17700", ""},
	}
	for _, c := range cases {
		if got := withFixedPort(c.host, c.port); got != c.want {
			t.Errorf("withFixedPort(%q, %q) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}
