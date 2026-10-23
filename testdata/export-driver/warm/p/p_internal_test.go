package p

import "testing"

func TestP(t *testing.T) {
	if P() != 1 {
		t.Fatal("p")
	}
}
