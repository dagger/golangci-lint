// Package helper is imported by app's external test.
package helper

import app "example.com/exportdriver"

// Twice doubles app's count.
func Twice() int { return 2 * app.Count() }
