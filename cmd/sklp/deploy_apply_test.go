package main

import (
	"reflect"
	"testing"
)

func TestNormalizeDeployApply(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"apply", "prod", "-space", "x"}, []string{"prod", "-space", "x"}},
		{[]string{"apply"}, []string{}},
		{[]string{"prod", "-space", "x"}, []string{"prod", "-space", "x"}},
		{[]string{"ls", "-space", "x"}, []string{"ls", "-space", "x"}},
		{[]string{"mesh", "ls"}, []string{"mesh", "ls"}},
		{nil, nil},
	} {
		if got := normalizeDeployApply(tc.in); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("normalizeDeployApply(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
