package domain

import (
	"testing"
	"time"
)

// TestNewNodeRequiresStack: a node fronts a stack, so it cannot exist without
// one — an empty stack id would orphan it from every access check.
func TestNewNodeRequiresStack(t *testing.T) {
	if _, err := NewNode("", "edge", "https://1.2.3.4:6464", []byte("tok"), ""); err == nil {
		t.Fatal("expected a missing stack_id to be rejected")
	}
}

func TestNewNodeRequiresHTTPS(t *testing.T) {
	if _, err := NewNode("s1", "edge", "http://1.2.3.4:6464", []byte("tok"), ""); err == nil {
		t.Fatal("expected a plain-http server_url to be rejected")
	}
}

func TestNewNodeRequiresToken(t *testing.T) {
	if _, err := NewNode("s1", "edge", "https://1.2.3.4:6464", nil, ""); err == nil {
		t.Fatal("expected a missing token to be rejected")
	}
}

func TestNewNodeTrimsTrailingSlash(t *testing.T) {
	n, err := NewNode("s1", "edge", "https://1.2.3.4:6464/", []byte("tok"), "")
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if n.ServerURL != "https://1.2.3.4:6464" {
		t.Fatalf("ServerURL = %q, want the trailing slash gone", n.ServerURL)
	}
}

func TestRecordFailureThenAppliedClearsError(t *testing.T) {
	n, err := NewNode("s1", "edge", "https://1.2.3.4:6464", []byte("tok"), "")
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	n.RecordFailure("unreachable")
	if n.LastError == "" || n.LastAppliedAt != nil {
		t.Fatalf("RecordFailure did not mark the node diverged: %+v", n)
	}
	n.RecordApplied(time.Now())
	if n.LastError != "" || n.LastAppliedAt == nil {
		t.Fatalf("RecordApplied did not clear the failure: %+v", n)
	}
}
