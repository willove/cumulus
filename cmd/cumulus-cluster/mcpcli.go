package main

// MCP stdio proxy (P8): local MCP clients (Claude Desktop, Cursor, …) speak
// newline-delimited JSON-RPC over stdin/stdout, while this suite's MCP face
// lives on a running serve's POST /mcp. The proxy bridges the two and owns
// NO store — Badger holds an exclusive directory lock, so a second process
// opening the same -data directory would fail; agent clients therefore spawn
// `cumulus-cluster mcp` and every JSON-RPC message is forwarded over HTTP.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const mcpDefaultURL = "http://127.0.0.1:8484/mcp"

// defaultMCPURL resolves the proxy target: $CLUS_MCP_URL, else the default
// serve address.
func defaultMCPURL() string {
	if v := strings.TrimSpace(os.Getenv("CLUS_MCP_URL")); v != "" {
		return v
	}
	return mcpDefaultURL
}

// runMCPProxy copies JSON-RPC lines from in to url and writes responses to
// out until in ends. Notifications (answered 202/empty by the server) produce
// no output line; transport failures surface as a JSON-RPC error on stderr.
func runMCPProxy(ctx context.Context, url string, in io.Reader, out io.Writer, errw io.Writer) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(line))
		if err != nil {
			fmt.Fprintf(errw, "mcp: build request: %v\n", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(errw, "mcp: POST %s: %v\n", url, err)
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			fmt.Fprintf(errw, "mcp: read response: %v\n", rerr)
			continue
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			continue // notification: accepted, no response
		}
		if _, werr := fmt.Fprintf(out, "%s\n", strings.TrimSpace(string(body))); werr != nil {
			return werr
		}
	}
	return sc.Err()
}
