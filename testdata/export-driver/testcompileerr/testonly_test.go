package testonly

import (
	_ "embed"
	"testing"
)

// go/types accepts embedding into an int; the compiler does not.
//
//go:embed data.txt
var want int

func TestOne(t *testing.T) {
	if One() != want {
		t.Fatal("one")
	}
}
