// Command naru-entry is the entrypoint of the test image a Machine or a Pod runs. With no arguments
// it prints one line and stays running, as mvmd's own end-to-end entrypoint does.
//
// The Kubernetes test (ADR 0031) gives it arguments, so that one image is the server and the client:
//
//	naru-entry serve <address>       answer every HTTP request with a line, and print each peer
//	naru-entry dial <url>...         fetch each URL until it answers, print one line per URL, and stay running
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// answer is what serve replies, and what dial looks for.
const answer = "naru91-entry-answer"

func main() {
	fmt.Println("hello from the entrypoint")
	switch {
	case len(os.Args) == 3 && os.Args[1] == "serve":
		serve(os.Args[2])
	case len(os.Args) > 2 && os.Args[1] == "dial":
		for _, u := range os.Args[2:] {
			dial(u)
		}
	}
	time.Sleep(time.Hour)
}

func serve(addr string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("naru91-entry: served %s\n", r.RemoteAddr)
		_, _ = io.WriteString(w, answer+"\n")
	})
	go func() {
		fmt.Printf("naru91-entry: serving %s\n", addr)
		srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second}
		fmt.Println("naru91-entry: serve ended:", srv.ListenAndServe())
		os.Exit(1)
	}()
}

// dial fetches u every two seconds for ten minutes, and prints "naru91-entry: dialed <u>" on the first answer.
func dial(u string) {
	client := &http.Client{Timeout: 3 * time.Second}
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		resp, err := client.Get(u)
		if err != nil {
			fmt.Printf("naru91-entry: dial %s: %v\n", u, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && string(body) == answer+"\n" {
			fmt.Printf("naru91-entry: dialed %s\n", u)
			return
		}
	}
	fmt.Printf("naru91-entry: gave up on %s\n", u)
}
