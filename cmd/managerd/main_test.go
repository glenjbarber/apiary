package main

import "testing"

func TestRunReset_WrongResetManagedPhraseDoesNothing(t *testing.T) {
	if err := runReset("not-the-phrase", "", "", "", "zroot/apiary", "apiary-", "apiary-", "/tmp/isos"); err == nil {
		t.Fatal("runReset() with wrong -reset-managed phrase = nil error, want a rejection")
	}
}

func TestRunReset_WrongFactoryResetPhraseDoesNothing(t *testing.T) {
	if err := runReset("", "not-the-phrase", "", "", "zroot/apiary", "apiary-", "apiary-", "/tmp/isos"); err == nil {
		t.Fatal("runReset() with wrong -factory-reset phrase = nil error, want a rejection")
	}
}

func TestRunReset_BothEmptyIsANoOp(t *testing.T) {
	if err := runReset("", "", "", "", "zroot/apiary", "apiary-", "apiary-", "/tmp/isos"); err != nil {
		t.Fatalf("runReset() with neither flag set = %v, want nil (this path shouldn't even be reached in main, but must be harmless)", err)
	}
}

func TestSplitCommaList(t *testing.T) {
	cases := map[string][]string{
		"":         nil,
		"a":        {"a"},
		"a,b,c":    {"a", "b", "c"},
		"a, b , c": {"a", "b", "c"},
		"a,,b":     {"a", "b"},
		"  ,  ,  ": nil,
	}
	for in, want := range cases {
		got := splitCommaList(in)
		if len(got) != len(want) {
			t.Errorf("splitCommaList(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitCommaList(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
