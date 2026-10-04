// Command tkprobe is a static HTTP health probe added to images that ship
// without a shell (e.g. the OpenTelemetry collector) so that every container
// of the stack has a real health check. Exit 0 on a 2xx answer.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: tkprobe URL")
		os.Exit(2)
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		os.Exit(1)
	}
}
