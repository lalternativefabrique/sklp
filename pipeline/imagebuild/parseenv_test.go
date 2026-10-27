package imagebuild

import (
	"reflect"
	"testing"
)

func TestParseEnv(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []EnvVar
	}{
		{"single pair", "A=1", []EnvVar{{"A", "1"}}},
		{"multi pair", "A=1 B=2 C=3", []EnvVar{{"A", "1"}, {"B", "2"}, {"C", "3"}}},
		{"legacy form", "GREETING hello world", []EnvVar{{"GREETING", "hello world"}}},
		{"quoted value with space", `A="hello world" B=2`, []EnvVar{{"A", "hello world"}, {"B", "2"}}},
		{"single-quoted value", `A='x y' B=z`, []EnvVar{{"A", "x y"}, {"B", "z"}}},
		{"empty value", "A= B=2", []EnvVar{{"A", ""}, {"B", "2"}}},
		{"continued multi (parser-joined)", "VITE_A=a1     VITE_B=b1     VITE_C=c1", []EnvVar{
			{"VITE_A", "a1"}, {"VITE_B", "b1"}, {"VITE_C", "c1"}}},
		{"empty", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseEnv(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseEnv(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}
