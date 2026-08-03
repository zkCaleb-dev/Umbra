package api

import "testing"

func TestRPCCursorRoundTrip(t *testing.T) {
	cases := []struct{ l, tx, ev uint32 }{
		{3944279, 16, 1},
		{1, 1, 0},
		{2147483647, 1048575, 4294967295}, // TOID ledgers are int32 by construction (Horizon TOID)
	}
	for _, c := range cases {
		s := formatRPCCursor2(c.l, c.tx, c.ev)
		l, tx, ev, err := parseRPCCursor(s)
		if err != nil {
			t.Fatalf("parse(%s): %v", s, err)
		}
		if l != c.l || tx != c.tx || ev != c.ev {
			t.Fatalf("round trip %v → %s → (%d,%d,%d)", c, s, l, tx, ev)
		}
	}
}

func TestRPCCursorMatchesRealRPCFormat(t *testing.T) {
	// Observed live on testnet: ledger 3944279, tx 16, event 1 has id
	// 0016940549311365120-0000000001.
	got := formatRPCCursor2(3944279, 16, 1)
	want := "0016940549311365120-0000000001"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestParseRPCCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "abc", "123", "12-ab", "-1-0", "1-2-3-4"} {
		if _, _, _, err := parseRPCCursor(bad); err == nil {
			t.Errorf("parseRPCCursor(%q) should fail", bad)
		}
	}
}

func TestMatchTopics(t *testing.T) {
	topics := []string{"A", "B", "C"}
	cases := []struct {
		filter []string
		want   bool
	}{
		{[]string{"A", "B", "C"}, true},
		{[]string{"*", "B", "C"}, true},
		{[]string{"A", "**"}, true},
		{[]string{"**"}, true},
		{[]string{"A", "B"}, false},         // length mismatch, no **
		{[]string{"A", "B", "C", "D"}, false},
		{[]string{"X", "B", "C"}, false},
		{[]string{"A", "*", "*"}, true},
	}
	for _, c := range cases {
		if got := matchTopics([][]string{c.filter}, topics); got != c.want {
			t.Errorf("filter %v: got %v, want %v", c.filter, got, c.want)
		}
	}
	if !matchTopics(nil, topics) {
		t.Error("empty filter set must match everything")
	}
}
