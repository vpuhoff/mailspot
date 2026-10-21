// loadtest sends bounded-size messages at a fixed overall rate, or as fast as possible.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("smtp", "127.0.0.1:1025", "SMTP address")
	web := flag.String("http", "http://127.0.0.1:8080", "HTTP URL for parallel search; empty disables")
	duration := flag.Duration("duration", time.Minute, "test duration")
	rate := flag.Int("rate", 600, "messages per minute; 0 = unlimited")
	workers := flag.Int("workers", 8, "concurrent SMTP clients")
	size := flag.Int("size", 4096, "approximate raw message size in bytes")
	flag.Parse()
	if *workers < 1 || *workers > 1024 || *rate < 0 || *rate > 1_000_000 || *size < 256 || *size > 100<<20 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "invalid parameters")
		os.Exit(2)
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, *duration)
	defer cancel()
	var sent, failed, searches, searchErrors, totalNanos, maxNanos atomic.Int64
	jobs := make(chan int64, *workers)
	var wg sync.WaitGroup
	started := time.Now()
	for worker := 0; worker < *workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				raw := payload(id, *size)
				start := time.Now()
				err := send(*addr, raw)
				elapsed := time.Since(start).Nanoseconds()
				if err != nil {
					if failed.Add(1) <= 5 {
						fmt.Fprintln(os.Stderr, err)
					}
				} else {
					sent.Add(1)
					totalNanos.Add(elapsed)
					for {
						old := maxNanos.Load()
						if elapsed <= old || maxNanos.CompareAndSwap(old, elapsed) {
							break
						}
					}
				}
			}
		}()
	}
	var readers sync.WaitGroup
	if *web != "" {
		readers.Add(1)
		go func() {
			defer readers.Done()
			client := http.Client{Timeout: 10 * time.Second}
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(*web, "/")+"/api/messages?q=loadtest&limit=50", nil)
					if req == nil {
						searchErrors.Add(1)
						continue
					}
					resp, err := client.Do(req)
					if err != nil {
						if ctx.Err() == nil {
							searchErrors.Add(1)
						}
						continue
					}
					_, e := io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 || e != nil {
						searchErrors.Add(1)
					} else {
						searches.Add(1)
					}
				}
			}
		}()
	}
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				fmt.Printf("elapsed=%s accepted=%d failed=%d search_ok=%d search_failed=%d\n", time.Since(started).Round(time.Second), sent.Load(), failed.Load(), searches.Load(), searchErrors.Load())
			}
		}
	}()
	var ticks <-chan time.Time
	if *rate > 0 {
		timer := time.NewTicker(time.Minute / time.Duration(*rate))
		defer timer.Stop()
		ticks = timer.C
	}
dispatch:
	for id := int64(0); ; id++ {
		if ticks != nil {
			select {
			case <-ctx.Done():
				break dispatch
			case <-ticks:
			}
		}
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- id:
		}
	}
	close(jobs)
	wg.Wait()
	readers.Wait()
	close(progressDone)
	elapsed := time.Since(started)
	avg := time.Duration(0)
	if sent.Load() > 0 {
		avg = time.Duration(totalNanos.Load() / sent.Load())
	}
	fmt.Printf("duration=%s accepted=%d failed=%d messages_per_minute=%.1f avg_smtp=%s max_smtp=%s search_ok=%d search_failed=%d\n", elapsed.Round(time.Millisecond), sent.Load(), failed.Load(), float64(sent.Load())/elapsed.Minutes(), avg, time.Duration(maxNanos.Load()), searches.Load(), searchErrors.Load())
	if failed.Load() > 0 || searchErrors.Load() > 0 || sent.Load() == 0 {
		os.Exit(1)
	}
}

func payload(id int64, size int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: loadtest@example.test\r\nTo: inbox@example.test\r\nSubject: loadtest %d\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n", id)
	for b.Len() < size {
		fmt.Fprintf(&b, "loadtest message %d %s\r\n", id, strings.Repeat("x", 64))
	}
	return b.Bytes()
}
func send(addr string, raw []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	host, _, _ := net.SplitHostPort(addr)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if err = client.Mail("loadtest@example.test"); err != nil {
		return err
	}
	if err = client.Rcpt("inbox@example.test"); err != nil {
		return err
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = data.Write(raw); err != nil {
		return err
	}
	if err = data.Close(); err != nil {
		return err
	}
	// DATA success means the message was accepted, even if the connection closes before QUIT.
	client.Quit()
	return nil
}
