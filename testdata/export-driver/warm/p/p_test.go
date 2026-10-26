package p_test

import (
	"testing"

	"example.com/warm/q"
)

// q imports p, so the test build recompiles q against p's test variant.
func TestQ(t *testing.T) {
	if q.Q() < 2 {
		t.Fatal("q")
	}
}
