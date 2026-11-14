// Command naru-init is the host's PID 1: the init core in lib/init/run. hostd stays a service it
// supervises (docs/architecture/adr/0039).
package main

import "github.com/garamsh/naru/lib/init/run"

func main() {
	run.Main()
}
