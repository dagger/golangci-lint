// Package compileerr has an error only the compiler reports.
package compileerr

import _ "embed"

// go/types accepts embedding into an int; the compiler does not.
//
//go:embed data.txt
var count int

// Count returns the embedded count.
func Count() int { return count }
