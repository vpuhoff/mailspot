package mailspot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

type Message struct {
	ID         int64  `json:"id"`
	Sender     string `json:"sender"`
	Recipients string `json:"recipients"`
	Subject    string `json:"subject"`
	Body       string `json:"body,omitempty"`
	ReceivedAt int64  `json:"received_at"`
	Size       int    `json:"size"`
	Truncated  bool   `json:"truncated"`
	Raw        []byte `json:"-"`
}

type Store struct{ writer, reader *sql.DB }

func OpenStore(path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "cache_size(-8192)")
	u.RawQuery = q.Encode()
	w, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	s := &Store{writer: w}
	fail := func(err error) (*Store, error) { s.Close(); return nil, err }
	_, err = w.Exec(`PRAGMA auto_vacuum=INCREMENTAL;
 PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS messages (
 id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT NOT NULL, recipients TEXT NOT NULL,
 subject TEXT NOT NULL, body TEXT NOT NULL, received_at INTEGER NOT NULL, size INTEGER NOT NULL,
 truncated INTEGER NOT NULL, raw BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS messages_received ON messages(received_at);
 CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(sender, recipients, subject, body, content='messages', content_rowid='id', tokenize='trigram');
 CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
 INSERT INTO messages_fts(rowid,sender,recipients,subject,body) VALUES(new.id,new.sender,new.recipients,new.subject,new.body); END;
 CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
 INSERT INTO messages_fts(messages_fts,rowid,sender,recipients,subject,body) VALUES('delete',old.id,old.sender,old.recipients,old.subject,old.body); END;`)
	if err != nil {
		return fail(err)
	}
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	s.reader, err = sql.Open("sqlite", u.String())
	if err != nil {
		return fail(err)
	}
	s.reader.SetMaxOpenConns(4)
	if err = s.reader.Ping(); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *Store) Close() {
	if s.reader != nil {
		s.reader.Close()
	}
	if s.writer != nil {
		s.writer.Close()
	}
}
func (s *Store) Ping(ctx context.Context) error { return s.reader.PingContext(ctx) }
func (s *Store) Save(ctx context.Context, m *Message) error {
	res, err := s.writer.ExecContext(ctx, `INSERT INTO messages(sender,recipients,subject,body,received_at,size,truncated,raw) VALUES(?,?,?,?,?,?,?,?)`, m.Sender, m.Recipients, m.Subject, m.Body, m.ReceivedAt, m.Size, m.Truncated, m.Raw)
	if err == nil {
		m.ID, err = res.LastInsertId()
	}
	return err
}

var ErrQuery = errors.New("поиск: от 3 до 200 символов, без нулевых байтов")

func (s *Store) List(ctx context.Context, query string, before int64, limit int) ([]Message, error) {
	query = strings.TrimSpace(query)
	if query != "" && (utf8.RuneCountInString(query) < 3 || utf8.RuneCountInString(query) > 200 || strings.ContainsRune(query, 0)) {
		return nil, ErrQuery
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	stmt := `SELECT id,sender,recipients,subject,received_at,size,truncated FROM messages WHERE 1=1`
	args := []any{}
	if before > 0 {
		stmt += ` AND id < ?`
		args = append(args, before)
	}
	if query != "" {
		stmt += ` AND id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`
		args = append(args, `"`+strings.ReplaceAll(query, `"`, `""`)+`"`)
	}
	stmt += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Message, 0, limit)
	for rows.Next() {
		var m Message
		if err = rows.Scan(&m.ID, &m.Sender, &m.Recipients, &m.Subject, &m.ReceivedAt, &m.Size, &m.Truncated); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
func (s *Store) Get(ctx context.Context, id int64, raw bool) (Message, error) {
	var m Message
	columns := `id,sender,recipients,subject,body,received_at,size,truncated`
	dest := []any{&m.ID, &m.Sender, &m.Recipients, &m.Subject, &m.Body, &m.ReceivedAt, &m.Size, &m.Truncated}
	if raw {
		columns += `,raw`
		dest = append(dest, &m.Raw)
	}
	err := s.reader.QueryRowContext(ctx, `SELECT `+columns+` FROM messages WHERE id=?`, id).Scan(dest...)
	return m, err
}

// Each small transaction releases the writer for incoming mail.
func (s *Store) Cleanup(ctx context.Context, cutoff time.Time, maxMessages int64) (int64, error) {
	var total int64
	for {
		res, err := s.writer.ExecContext(ctx, `DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE received_at < ? ORDER BY received_at LIMIT 250)`, cutoff.UnixMilli())
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 250 {
			break
		}
		if err = cleanupPause(ctx); err != nil {
			return total, err
		}
	}
	// Find the oldest retained ID once, then delete older records in bounded chunks.
	var boundary int64
	err := s.reader.QueryRowContext(ctx, `SELECT id FROM messages ORDER BY id DESC LIMIT 1 OFFSET ?`, maxMessages-1).Scan(&boundary)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return total, err
	}
	if err == nil {
		for {
			res, err := s.writer.ExecContext(ctx, `DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE id < ? ORDER BY id LIMIT 250)`, boundary)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
			if n < 250 {
				break
			}
			if err = cleanupPause(ctx); err != nil {
				return total, err
			}
		}
	}
	if _, err = s.writer.ExecContext(ctx, `PRAGMA incremental_vacuum(256)`); err != nil {
		return total, fmt.Errorf("vacuum: %w", err)
	}
	_, err = s.writer.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return total, err
}
func cleanupPause(ctx context.Context) error {
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
