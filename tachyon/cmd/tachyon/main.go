package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("TACHYON_HTTP_ADDR", "http://127.0.0.1:8645"), "daemon base URL")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		args = []string{"health"}
	}

	cmd := args[0]
	var path string
	var body any
	switch cmd {
	case "health", "healthz":
		if err := get(*addr, "/healthz", nil); err != nil {
			fatal(err)
		}
		return
	case "compile":
		path = "/v1/compile"
		body = readJSONStdin()
	case "test":
		path = "/v1/test"
		body = readJSONStdin()
	case "simulate":
		path = "/v1/simulate"
		body = readJSONStdin()
	case "deploy":
		path = "/v1/deploy"
		body = readJSONStdin()
	case "call":
		path = "/v1/call"
		body = readJSONStdin()
	case "chains":
		if err := get(*addr, "/v1/chains", nil); err != nil {
			fatal(err)
		}
		return
	default:
		fatal(fmt.Errorf("unknown command %q", cmd))
	}

	if err := post(*addr, path, body); err != nil {
		fatal(err)
	}
}

func get(base, path string, _ any) error {
	base = strings.TrimRight(base, "/")
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return printResp(resp)
}

func post(base, path string, body any) error {
	base = strings.TrimRight(base, "/")
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	req, err := http.NewRequest(http.MethodPost, base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return printResp(resp)
}

func printResp(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Println(string(body))
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func readJSONStdin() any {
	b, err := io.ReadAll(os.Stdin)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		fatal(err)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "tachyon: %v\n", err)
	os.Exit(1)
}
