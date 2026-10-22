package app_test

import (
	"testing"

	"example.com/exportdriver/helper"
)

// helper imports app, so the test build recompiles it against app's test
// variant: "helper [example.com/exportdriver.test]".
func TestHelper(t *testing.T) {
	if helper.Twice() != 6 {
		t.Fatal("twice")
	}
}
