package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"opencode2api/internal/warp"
)

func attempt(n int) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.Level(100)}))
	start := time.Now()
	pool, err := warp.NewWithOptions(ctx, log, warp.Options{Size: 1, Stack: warp.StackIPv6})
	if err != nil {
		fmt.Printf("try %d: START FAIL (%s): %v\n", n, time.Since(start).Round(time.Millisecond), err)
		return
	}
	defer pool.Close()
	lease, err := pool.Acquire(ctx)
	if err != nil {
		fmt.Printf("try %d: ACQUIRE FAIL (%s): %v\n", n, time.Since(start).Round(time.Millisecond), err)
		return
	}
	defer lease.Release()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: lease.DialContext}}
	resp, err := client.Get("https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		fmt.Printf("try %d: HTTP FAIL (%s): %v\n", n, time.Since(start).Round(time.Millisecond), err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	fmt.Printf("try %d: OK %d in %s :: %s", n, resp.StatusCode, time.Since(start).Round(time.Millisecond), firstLines(string(body), 3))
}

func firstLines(s string, n int) string {
	out := ""
	line := 0
	for _, r := range s {
		out += string(r)
		if r == '\n' {
			line++
			if line >= n {
				return out
			}
		}
	}
	return out
}

func main() {
	for i := 1; i <= 3; i++ {
		attempt(i)
		time.Sleep(2 * time.Second)
	}
}
