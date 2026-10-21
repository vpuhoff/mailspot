package mailspot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	netsmtp "net/smtp"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func save(t *testing.T, s *Store, subject string, at time.Time) *Message {
	t.Helper()
	m := &Message{Sender: "sender@example.test", Recipients: "target@example.test", Subject: subject, Body: "Проверка поиска needle", ReceivedAt: at.UnixMilli(), Raw: []byte("Subject: " + subject + "\r\n\r\nbody"), Size: 40}
	if err := s.Save(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSearchPaginationAndCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	old := save(t, s, "old-only-token", now.Add(-4*time.Hour))
	first := save(t, s, `Тест "quoted" 100%_done`, now)
	second := save(t, s, "second", now)
	for _, q := range []string{"ПРОВЕРКА", "nder@exa", "quoted", "100%_", `"quoted"`} {
		rows, err := s.List(ctx, q, 0, 50)
		if err != nil || len(rows) == 0 {
			t.Fatalf("query %q: rows=%v err=%v", q, rows, err)
		}
	}
	rows, err := s.List(ctx, "", second.ID, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("pagination: %v %v", rows, err)
	}
	if _, err = s.List(ctx, "ab", 0, 50); !errors.Is(err, ErrQuery) {
		t.Fatal("short query accepted")
	}
	if _, err = s.List(ctx, `" OR * --`, 0, 50); err != nil {
		t.Fatal(err)
	}
	n, err := s.Cleanup(ctx, now.Add(-time.Hour), 100)
	if err != nil || n != 1 {
		t.Fatalf("age cleanup: %d %v", n, err)
	}
	if _, err = s.Get(ctx, old.ID, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old message remains")
	}
	rows, err = s.List(ctx, "old-only-token", 0, 50)
	if err != nil || len(rows) != 0 {
		t.Fatalf("stale search: %v %v", rows, err)
	}
	n, err = s.Cleanup(ctx, now.Add(-time.Hour), 1)
	if err != nil || n != 1 {
		t.Fatalf("count cleanup: %d %v", n, err)
	}
	rows, err = s.List(ctx, "needle", 0, 50)
	if err != nil || len(rows) != 1 || rows[0].ID != second.ID {
		t.Fatalf("search after cleanup: %v %v", rows, err)
	}
}

func TestPersistenceAndRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	m := save(t, s, "persistent", time.Now())
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(context.Background(), m.ID, true)
	if err != nil || !bytes.Equal(got.Raw, m.Raw) {
		t.Fatalf("reopen: %v %v", got, err)
	}
}

func TestMIME(t *testing.T) {
	raw := []byte("Subject: =?UTF-8?B?0J/RgNC40LLQtdGC?=\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("Привет мир")) + "\r\n--x\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=secret.txt\r\n\r\nattachment-secret\r\n--x--\r\n")
	m := ParseMessage(raw, 1024)
	if m.Subject != "Привет" || !strings.Contains(m.Body, "Привет мир") || strings.Contains(m.Body, "attachment-secret") || !bytes.Equal(m.Raw, raw) {
		t.Fatalf("MIME: %+v", m)
	}
	m = ParseMessage([]byte("Content-Type: text/plain; charset=windows-1251\r\n\r\n\xcf\xf0\xe8\xe2\xe5\xf2"), 1024)
	if m.Body != "Привет" {
		t.Fatalf("charset: %q", m.Body)
	}
	m = ParseMessage([]byte("Subject: large\r\n\r\n"+strings.Repeat("a", 2048)), 100)
	if !m.Truncated || len(m.Body) != 100 {
		t.Fatalf("body cap: %d %v", len(m.Body), m.Truncated)
	}
}

func TestHTTP(t *testing.T) {
	s := testStore(t)
	m := save(t, s, "<script>alert(1)</script>", time.Now())
	h := Handler(s, Config{Retention: 72 * time.Hour})
	for _, tc := range []struct {
		path   string
		status int
	}{{"/", 200}, {fmt.Sprintf("/?id=%d", m.ID), 200}, {"/api/messages?q=needle", 200}, {"/api/messages?q=ab", 400}, {fmt.Sprintf("/messages/%d/raw", m.ID), 200}, {"/api/messages/999", 404}, {"/healthz", 200}, {"/missing", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if w.Header().Get("Content-Type") == "text/html; charset=utf-8" && strings.Contains(w.Body.String(), "<script>") {
			t.Fatal("unescaped HTML")
		}
	}
}

func TestSMTPConcurrentSearchCleanup(t *testing.T) {
	s := testStore(t)
	c := Config{MaxMessageBytes: 4096, MaxBodyBytes: 2048, MaxConnections: 16}
	server := NewSMTP(c, s)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(LimitListener(l, 16)) }()
	defer func() { server.Close(); <-done }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	maintenance := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				maintenance <- nil
				return
			default:
			}
			if _, e := s.Cleanup(ctx, time.Now().Add(-time.Hour), 1000); e != nil {
				if ctx.Err() != nil {
					maintenance <- nil
				} else {
					maintenance <- e
				}
				return
			}
			if _, e := s.List(ctx, "payload", 0, 50); e != nil {
				if ctx.Err() != nil {
					maintenance <- nil
				} else {
					maintenance <- e
				}
				return
			}
		}
	}()
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				raw := []byte(fmt.Sprintf("Subject: worker-%d-%d\r\n\r\npayload", worker, i))
				if e := netsmtp.SendMail(l.Addr().String(), nil, "from@example.test", []string{"a@example.test", "b@example.test"}, raw); e != nil {
					failures <- e
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Error(e)
	}
	cancel()
	if e := <-maintenance; e != nil {
		t.Error(e)
	}
	var count int
	if err = s.reader.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 160 {
		t.Fatalf("accepted count=%d err=%v", count, err)
	}
	if err = netsmtp.SendMail(l.Addr().String(), nil, "from@example.test", []string{"a@example.test"}, []byte("Subject: large\r\n\r\n"+strings.Repeat("a\r\n", 3000))); err == nil {
		t.Fatal("oversized message accepted")
	}
	s.reader.QueryRow(`SELECT count(*) FROM messages`).Scan(&count)
	if count != 160 {
		t.Fatal("oversized message stored")
	}
}

func TestSMTPStorageFailure(t *testing.T) {
	s := testStore(t)
	s.writer.Close()
	session := &session{backend: &Backend{Store: s, Config: Config{MaxMessageBytes: 1024, MaxBodyBytes: 1024}}}
	if err := session.Data(strings.NewReader("Subject: fail\r\n\r\nbody")); err == nil || !strings.Contains(err.Error(), "451") {
		t.Fatalf("must not acknowledge failed persistence: %v", err)
	}
}

func TestConnectionLimit(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	limited := LimitListener(listener, 1)
	first, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	accepted, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	next := make(chan net.Conn, 1)
	go func() { c, _ := limited.Accept(); next <- c }()
	second, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(time.Second))
	reply, err := io.ReadAll(second)
	if err != nil || !strings.HasPrefix(string(reply), "421") {
		t.Fatalf("limit reply %q: %v", reply, err)
	}
	accepted.Close()
	accepted.Close()
	third, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	select {
	case accepted = <-next:
		if accepted == nil {
			t.Fatal("accept failed")
		}
	case <-time.After(time.Second):
		t.Fatal("connection slot not released")
	}
	limited.CloseActive()
	third.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = third.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("active connection remains: %v", err)
	}
}
