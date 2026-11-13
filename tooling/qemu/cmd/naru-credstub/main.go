// Command naru-credstub is the interpreter of a test's stand-in credential helper. The image has
// no shell, so a stand-in docker-credential-<name> is a file in spec.files that starts
// "#!/var/lib/naru/inputs/bin/naru-credstub" and whose second line is "user:password". The kernel
// runs this program with the file's path and the helper's argument; "get" prints that user and
// password in the form a Docker credential helper does.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 3 || os.Args[2] != "get" {
		fmt.Fprintln(os.Stderr, "naru-credstub: want SCRIPT get")
		os.Exit(2)
	}
	server, _ := io.ReadAll(bufio.NewReader(os.Stdin))
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "naru-credstub:", err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Scan() // the interpreter line
	if !sc.Scan() {
		fmt.Fprintln(os.Stderr, "naru-credstub: the script has no second line")
		os.Exit(1)
	}
	user, pass, ok := strings.Cut(sc.Text(), ":")
	if !ok {
		fmt.Fprintln(os.Stderr, "naru-credstub: the second line is not user:password")
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
		"ServerURL": strings.TrimSpace(string(server)), "Username": user, "Secret": pass,
	})
}
