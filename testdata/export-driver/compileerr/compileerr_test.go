package compileerr

import "testing"

// An internal test: golangci-lint lints the package through its test
// variant, not the plain package.
func TestCount(t *testing.T) { _ = Count() }
