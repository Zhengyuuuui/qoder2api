package main

import (
	"testing"

	"qoder2api/account"
)

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
		ok   bool
	}{
		{"09:15", 9, 15, true},
		{"17:00", 17, 0, true},
		{"0:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{"25:00", 0, 0, false},
		{"09:60", 0, 0, false},
		{"abc", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		h, m, ok := parseHHMM(c.in)
		if ok != c.ok || (ok && (h != c.h || m != c.m)) {
			t.Errorf("parseHHMM(%q) = (%d,%d,%v), want (%d,%d,%v)", c.in, h, m, ok, c.h, c.m, c.ok)
		}
	}
}

func TestCheckinTimesDefaultAndConfigured(t *testing.T) {
	if got := CheckinTimes(nil); len(got) != 1 || got[0] != "10:00" {
		t.Errorf("default = %v, want [10:00]", got)
	}
	s := &account.Settings{AutoCheckinTimes: []string{"09:15", "17:00"}}
	got := CheckinTimes(s)
	if len(got) != 2 || got[0] != "09:15" || got[1] != "17:00" {
		t.Errorf("configured = %v, want [09:15 17:00]", got)
	}
}

func TestCheckinHostForRegion(t *testing.T) {
	if got := checkinHostFor(account.RegionCN); got != "openapi.qoder.com.cn" {
		t.Errorf("cn host = %q", got)
	}
	if got := checkinHostFor(account.RegionGlobal); got != "openapi.qoder.sh" {
		t.Errorf("global host = %q", got)
	}
}
