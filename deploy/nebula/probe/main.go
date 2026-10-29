// Command nebula-probe is the health check of the NebulaGraph images
// (deploy/nebula). Their distroless base has no curl or shell, so the
// compose health checks and the chart's exec probes run this instead:
//
//	nebula-probe HOST:PORT
//
// asks http://HOST:PORT/status and exits 0 only on a 2xx answer.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: nebula-probe HOST:PORT")
		os.Exit(2)
	}
	if err := probe(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func probe(hostport string) error {
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		return fmt.Errorf("want HOST:PORT: %w", err)
	}
	url := "http://" + hostport + "/status"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // the address comes from our own probe definitions, never from a request
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req) //nolint:gosec // the address comes from our own probe definitions, never from a request
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}
