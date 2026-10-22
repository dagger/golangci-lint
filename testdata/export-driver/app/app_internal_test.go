package app

import "testing"

func TestCount(t *testing.T) {
	if Count() != 3 {
		t.Fatal("count")
	}
}
