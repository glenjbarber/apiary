package jail

import (
	"context"
	"strings"
	"testing"
)

func TestProtectedJailRejectedBeforeExec(t *testing.T) {
	for _, tc := range []struct{ prefix, name string }{
		{"", "timemachine"}, {"apiary-", "timemachine"}, {"time", "machine"},
	} {
		m := New(tc.prefix)
		ctx := context.Background()
		_, existsErr := m.JailExists(ctx, tc.name)
		_, infoErr := m.JailInfo(ctx, tc.name)
		for _, err := range []error{existsErr, infoErr,
			m.CreateJail(ctx, tc.name, Config{Path: "/unused"}), m.RemoveJail(ctx, tc.name)} {
			if err == nil || !strings.Contains(err.Error(), "protected") {
				t.Fatalf("prefix=%q name=%q: expected protected error, got %v", tc.prefix, tc.name, err)
			}
		}
	}
}

func TestListFiltersProtectedJail(t *testing.T) {
	for _, prefix := range []string{"", "apiary-"} {
		names := New(prefix).managedNames("name=timemachine\nname=apiary-timemachine\nname=" + prefix + "test-jail-01")
		for _, name := range names {
			if name == "timemachine" {
				t.Fatal("protected jail entered managed inventory")
			}
		}
		if !strings.Contains(strings.Join(names, ","), "test-jail-01") {
			t.Fatal("ordinary jail missing")
		}
	}
}
