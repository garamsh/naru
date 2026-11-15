package workspace

import "fmt"

// A deliberate go vet failure for the red-check demonstration of #209.
func demoRed() string { return fmt.Sprintf("%d", "not a number") }
