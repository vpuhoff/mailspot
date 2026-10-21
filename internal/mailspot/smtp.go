package mailspot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
)

type Backend struct {
	Store  *Store
	Config Config
}

func (b *Backend) NewSession(_ *smtp.Conn) (smtp.Session, error) { return &session{backend: b}, nil }

type session struct {
	backend *Backend
	from    string
	to      []string
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error { s.from = from; s.to = nil; return nil }
func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error   { s.to = append(s.to, to); return nil }
func (s *session) Reset()                                      { s.from = ""; s.to = nil }
func (s *session) Logout() error                               { return nil }
func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(r, s.backend.Config.MaxMessageBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > s.backend.Config.MaxMessageBytes {
		return &smtp.SMTPError{Code: 552, Message: "message too large"}
	}
	m := ParseMessage(raw, s.backend.Config.MaxBodyBytes)
	m.Sender = s.from
	m.Recipients = strings.Join(s.to, ", ")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = s.backend.Store.Save(ctx, &m); err != nil {
		slog.Error("store message", "error", err)
		return &smtp.SMTPError{Code: 451, Message: "storage unavailable, retry later"}
	}
	return nil
}

func ParseMessage(raw []byte, bodyLimit int) Message {
	m := Message{Raw: raw, Size: len(raw), ReceivedAt: time.Now().UTC().UnixMilli(), Subject: "Без темы"}
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if r == nil {
		m.Body = "Не удалось разобрать MIME; скачайте исходник .eml."
		m.Truncated = true
		return m
	}
	defer r.Close()
	if subject, e := r.Header.Subject(); e == nil && subject != "" {
		m.Subject = subject
	}
	var body strings.Builder
	for parts := 0; parts < 1000; parts++ {
		part, e := r.NextPart()
		if e == io.EOF {
			break
		}
		if part == nil {
			m.Truncated = true
			break
		}
		if e != nil {
			m.Truncated = true
		}
		h, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		typ, _, _ := h.ContentType()
		if typ != "text/plain" && typ != "text/html" {
			continue
		}
		remaining := bodyLimit - body.Len()
		if remaining <= 0 {
			m.Truncated = true
			break
		}
		data, e := io.ReadAll(io.LimitReader(part.Body, int64(remaining)+1))
		if e != nil {
			m.Truncated = true
		}
		if len(data) > remaining {
			data = data[:remaining]
			m.Truncated = true
		}
		// HTML is stored and displayed as escaped text, never executed.
		body.Write(data)
		if m.Truncated {
			break
		}
		if body.Len() < bodyLimit {
			body.WriteByte('\n')
		}
		if parts == 999 {
			m.Truncated = true
		}
	}
	m.Body = strings.ToValidUTF8(strings.TrimSpace(body.String()), "�")
	if !utf8.ValidString(m.Subject) {
		m.Subject = strings.ToValidUTF8(m.Subject, "�")
	}
	if err != nil {
		m.Truncated = true
	}
	return m
}

func NewSMTP(c Config, store *Store) *smtp.Server {
	s := smtp.NewServer(&Backend{Store: store, Config: c})
	s.Addr = c.SMTPAddr
	s.Domain = "mailspot.local"
	s.MaxMessageBytes = c.MaxMessageBytes
	s.MaxRecipients = 100
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 60 * time.Second
	return s
}

// Reject excess connections immediately; a slow sender cannot grow memory without bound.
type LimitedListener struct {
	net.Listener
	slots     chan struct{}
	mu        sync.Mutex
	active    map[*limitedConn]struct{}
	closed    bool
	Rejection string
}

func LimitListener(l net.Listener, n int) *LimitedListener {
	return &LimitedListener{Listener: l, slots: make(chan struct{}, n), active: make(map[*limitedConn]struct{}), Rejection: "421 too many connections, retry later\r\n"}
}
func (l *LimitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			tracked := &limitedConn{Conn: c}
			tracked.release = func() { l.mu.Lock(); delete(l.active, tracked); l.mu.Unlock(); <-l.slots }
			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				tracked.Close()
				return nil, net.ErrClosed
			}
			l.active[tracked] = struct{}{}
			l.mu.Unlock()
			return tracked, nil
		default:
			c.SetWriteDeadline(time.Now().Add(time.Second))
			fmt.Fprint(c, l.Rejection)
			c.Close()
		}
	}
}

// CloseActive is used after the graceful shutdown deadline has elapsed.
func (l *LimitedListener) CloseActive() {
	l.mu.Lock()
	l.closed = true
	connections := make([]*limitedConn, 0, len(l.active))
	for c := range l.active {
		connections = append(connections, c)
	}
	l.mu.Unlock()
	for _, c := range connections {
		c.Close()
	}
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
