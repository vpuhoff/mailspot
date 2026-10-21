package mailspot

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	SMTPAddr, HTTPAddr, DBPath   string
	Retention, CleanupInterval   time.Duration
	MaxMessages, MaxMessageBytes int64
	MaxConnections, MaxBodyBytes int
}

func LoadConfig() (Config, error) {
	c := Config{SMTPAddr: "127.0.0.1:1025", HTTPAddr: "127.0.0.1:8080", DBPath: "data/mailspot.db", Retention: 72 * time.Hour, CleanupInterval: time.Minute, MaxMessages: 3_000_000, MaxMessageBytes: 10 << 20, MaxConnections: 32, MaxBodyBytes: 256 << 10}
	for key, ptr := range map[string]*string{"SMTP_ADDR": &c.SMTPAddr, "HTTP_ADDR": &c.HTTPAddr, "DB_PATH": &c.DBPath} {
		if v, ok := os.LookupEnv(key); ok {
			if v == "" {
				return c, fmt.Errorf("%s must not be empty", key)
			}
			*ptr = v
		}
	}
	for key, ptr := range map[string]*time.Duration{"RETENTION": &c.Retention, "CLEANUP_INTERVAL": &c.CleanupInterval} {
		if v, ok := os.LookupEnv(key); ok {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return c, fmt.Errorf("%s must be a positive duration, e.g. 72h", key)
			}
			*ptr = d
		}
	}
	for key, ptr := range map[string]*int64{"MAX_MESSAGES": &c.MaxMessages, "MAX_MESSAGE_BYTES": &c.MaxMessageBytes} {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s must be a positive integer", key)
			}
			*ptr = n
		}
	}
	for key, ptr := range map[string]*int{"MAX_CONNECTIONS": &c.MaxConnections, "MAX_BODY_BYTES": &c.MaxBodyBytes} {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return c, fmt.Errorf("%s must be a positive integer", key)
			}
			*ptr = n
		}
	}
	if c.MaxMessageBytes > 100<<20 || c.MaxBodyBytes > 10<<20 || c.MaxConnections > 1024 {
		return c, fmt.Errorf("limits: message <= 100 MiB, indexed body <= 10 MiB, connections <= 1024")
	}
	return c, nil
}
