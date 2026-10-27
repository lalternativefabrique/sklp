package main

import "testing"

func TestValidateTarget(t *testing.T) {
	tests := []struct {
		target  string
		wantErr bool
	}{
		{"space-nats/nats:4222", false},
		{"space-nats/nats", true},
		{"nats:4222", true},
		{"", true},
		{"space-nats/nats:", true},
	}
	for _, tt := range tests {
		err := validateTarget(tt.target)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateTarget(%q) error = %v, wantErr %v", tt.target, err, tt.wantErr)
		}
	}
}
