package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"mailspot/internal/mailspot"
)

func main() {
	if err := run(); err != nil {
		slog.Error("mailspot stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	c, err := mailspot.LoadConfig()
	if err != nil {
		return err
	}
	store, err := mailspot.OpenStore(c.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()
	smtpListener, err := net.Listen("tcp", c.SMTPAddr)
	if err != nil {
		return err
	}
	defer smtpListener.Close()
	httpListener, err := net.Listen("tcp", c.HTTPAddr)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	smtpServer := mailspot.NewSMTP(c, store)
	httpServer := &http.Server{Handler: mailspot.Handler(store, c), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cleanCtx, cancelCleanup := context.WithCancel(context.Background())
	var cleanWG sync.WaitGroup
	cleanWG.Add(1)
	go func() {
		defer cleanWG.Done()
		ticker := time.NewTicker(c.CleanupInterval)
		defer ticker.Stop()
		for {
			n, e := store.Cleanup(cleanCtx, time.Now().Add(-c.Retention), c.MaxMessages)
			if e != nil && cleanCtx.Err() == nil {
				slog.Error("cleanup", "error", e)
			} else if n > 0 {
				slog.Info("cleanup", "deleted", n)
			}
			select {
			case <-cleanCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	errs := make(chan error, 2)
	limitedSMTP := mailspot.LimitListener(smtpListener, c.MaxConnections)
	limitedHTTP := mailspot.LimitListener(httpListener, 64)
	limitedHTTP.Rejection = "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"
	go func() { errs <- smtpServer.Serve(limitedSMTP) }()
	go func() { errs <- httpServer.Serve(limitedHTTP) }()
	slog.Info("mailspot started", "smtp", smtpListener.Addr(), "http", httpListener.Addr(), "retention", c.Retention, "max_messages", c.MaxMessages)
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	cancelCleanup()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if e := smtpServer.Shutdown(shutdownCtx); e != nil {
			limitedSMTP.CloseActive()
		}
	}()
	go func() {
		defer wg.Done()
		if e := httpServer.Shutdown(shutdownCtx); e != nil {
			httpServer.Close()
		}
	}()
	wg.Wait()
	cleanWG.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
