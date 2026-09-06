// Command load is a dependency-free load generator for the protocol proxy.
// It measures requests/sec, latency percentiles (p50/p95/p99), first-SSE-
// event latency for streaming, and error counts at a given concurrency.
//
// Usage:
//
//	go run ./tests/load -url http://127.0.0.1:8787/v1/responses \
//	    -concurrency 100 -duration 30s -stream
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var (
		target     = flag.String("url", "http://127.0.0.1:8787/v1/responses", "proxy endpoint")
		concurrent = flag.Int("concurrency", 10, "parallel workers")
		duration   = flag.Duration("duration", 10*time.Second, "test duration")
		stream     = flag.Bool("stream", false, "streaming mode (measures first-byte latency)")
		tools      = flag.Int("tools", 0, "attach N tool definitions")
		model      = flag.String("model", "bench-model", "model name")
	)
	flag.Parse()

	body := buildBody(*stream, *tools, *model)
	client := &http.Client{Timeout: 0}

	var (
		total      atomic.Int64
		errors     atomic.Int64
		bytesOut   atomic.Int64
		latencies  sync.Map // worker -> *[]float64, merged at the end
		firstBytes []float64
		fbMu       sync.Mutex
	)

	deadline := time.Now().Add(*duration)
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < *concurrent; w++ {
		wg.Add(1)
		lats := &[]float64{}
		latencies.Store(w, lats)
		go func(w int, lats *[]float64) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				t0 := time.Now()
				req, _ := http.NewRequest("POST", *target, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer bench")
				resp, err := client.Do(req)
				if err != nil {
					errors.Add(1)
					continue
				}
				var first time.Duration
				if *stream {
					sc := bufio.NewScanner(resp.Body)
					sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
					if sc.Scan() {
						first = time.Since(t0)
						fbMu.Lock()
						firstBytes = append(firstBytes, first.Seconds())
						fbMu.Unlock()
					}
					for sc.Scan() {
					}
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					errors.Add(1)
					continue
				}
				bytesOut.Add(n)
				total.Add(1)
				*lats = append(*lats, time.Since(t0).Seconds())
			}
		}(w, lats)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	var all []float64
	latencies.Range(func(_, v any) bool {
		all = append(all, *v.(*[]float64)...)
		return true
	})
	sort.Float64s(all)
	sort.Float64s(firstBytes)

	fmt.Printf("target=%s concurrency=%d duration=%s stream=%v\n", *target, *concurrent, *duration, *stream)
	fmt.Printf("requests:     %d (%.1f req/s)\n", total.Load(), float64(total.Load())/elapsed)
	fmt.Printf("errors:       %d\n", errors.Load())
	fmt.Printf("bytes out:    %d (%.1f MiB/s)\n", bytesOut.Load(), float64(bytesOut.Load())/elapsed/1024/1024)
	if len(all) > 0 {
		fmt.Printf("latency p50:  %s\n", pct(all, 50))
		fmt.Printf("latency p95:  %s\n", pct(all, 95))
		fmt.Printf("latency p99:  %s\n", pct(all, 99))
	}
	if len(firstBytes) > 0 {
		fmt.Printf("first byte p50: %s\n", pct(firstBytes, 50))
		fmt.Printf("first byte p95: %s\n", pct(firstBytes, 95))
		fmt.Printf("first byte p99: %s\n", pct(firstBytes, 99))
	}
	if errors.Load() > 0 {
		os.Exit(1)
	}
}

func pct(sorted []float64, p float64) time.Duration {
	idx := int(float64(len(sorted)-1) * p / 100)
	return time.Duration(sorted[idx] * float64(time.Second))
}

func buildBody(stream bool, tools int, model string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"model":%q,"stream":%t,"input":"Write a short paragraph about proxies."`, model, stream)
	for i := 0; i < tools; i++ {
		if i == 0 {
			b.WriteString(`,"tools":[`)
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"function","name":"tool_%d","description":"bench tool","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}`, i)
	}
	if tools > 0 {
		b.WriteString(`]`)
	}
	b.WriteString(`}`)
	return b.Bytes()
}
