// Package q uses p.
package q

import "example.com/warm/p"

// Q returns more than P.
func Q() int { return p.P() + 1 }
