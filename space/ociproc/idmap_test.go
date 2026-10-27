package ociproc

import "testing"

func TestRangeAboveRootFromMap(t *testing.T) {
	cases := map[string]int{
		"         0          0 4294967295\n":                                   0,
		"         0       1000          1\n":                                   0,
		"         0          0          1\n         1     100000      65536\n": 65536,
		"         0       1000          1\n         1     100000      65536\n": 65536,
		"         0          0      65536\n":                                   65535,
		"":                                                                     0,
	}
	for in, want := range cases {
		if got := rangeAboveRootFromMap(in); got != want {
			t.Errorf("%q: got %d, want %d", in, got, want)
		}
	}
}
