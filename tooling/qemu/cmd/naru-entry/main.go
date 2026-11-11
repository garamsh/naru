// Command naru-entry is the entrypoint of the test image a Machine runs: it
// prints one line and stays running, as mvmd's own end-to-end entrypoint does.
package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Println("hello from the entrypoint")
	time.Sleep(time.Hour)
}
