package mailspot

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed inbox.html
var inboxHTML string
var page = template.Must(template.New("inbox").Funcs(template.FuncMap{
	"date": func(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05 UTC") },
}).Parse(inboxHTML))

type pageData struct {
	Messages     []Message
	Selected     *Message
	Query, Error string
	Before, Next int64
	Retention    string
}

func Handler(store *Store, c Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		messages, err := store.List(ctx, r.URL.Query().Get("q"), before, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		var next int64
		if len(messages) > 0 {
			next = messages[len(messages)-1].ID
		}
		writeJSON(w, map[string]any{"messages": messages, "next_before": next})
	})
	mux.HandleFunc("GET /api/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		m, err := getMessage(store, r, false)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("GET /messages/{id}/raw", func(w http.ResponseWriter, r *http.Request) {
		m, err := getMessage(store, r, true)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="message-%d.eml"`, m.ID))
		w.Write(m.Raw)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		data := pageData{Query: strings.TrimSpace(r.URL.Query().Get("q")), Retention: c.Retention.String()}
		data.Before, _ = strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		var err error
		data.Messages, err = store.List(ctx, data.Query, data.Before, 50)
		if err != nil {
			if errors.Is(err, ErrQuery) {
				data.Error = err.Error()
			} else {
				writeError(w, err)
				return
			}
		}
		if len(data.Messages) == 50 {
			data.Next = data.Messages[len(data.Messages)-1].ID
		}
		if id, e := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64); e == nil && id > 0 {
			m, e := store.Get(ctx, id, false)
			if e != nil {
				writeError(w, e)
				return
			}
			data.Selected = &m
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, data); err != nil {
			slog.Error("render page", "error", err)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func getMessage(s *Store, r *http.Request, raw bool) (Message, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return Message{}, sql.ErrNoRows
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	return s.Get(ctx, id, raw)
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("json response", "error", err)
	}
}
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, "Письмо не найдено или уже удалено", 404)
	case errors.Is(err, ErrQuery):
		http.Error(w, err.Error(), 400)
	default:
		slog.Error("http request", "error", err)
		http.Error(w, "Хранилище временно недоступно", 503)
	}
}
