package main

import "testing"

func TestVerifySHA256(t *testing.T) {
	if err := verifySHA256("abc", "abc"); err != nil {
		t.Fatalf("matching hashes should pass: %v", err)
	}
	if err := verifySHA256("abc", "def"); err == nil {
		t.Fatal("mismatched hashes must error")
	}
}
