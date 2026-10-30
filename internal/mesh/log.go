package mesh

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// logWriter receives NetBird's log output (logrus text format, one entry
// per line) and forwards it to slog with the matching level.
type logWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func newLogWriter(l *slog.Logger) *logWriter { return &logWriter{log: l} }

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			w.emit(line)
		}
	}
	return len(p), nil
}

func (w *logWriter) emit(line string) {
	fields := parseLogfmt(line)
	level := slog.LevelInfo
	switch strings.ToLower(fields["level"]) {
	case "trace", "debug":
		level = slog.LevelDebug
	case "warning", "warn":
		level = slog.LevelWarn
	case "error", "fatal", "panic":
		level = slog.LevelError
	}
	msg, ok := fields["msg"]
	if !ok {
		w.log.Log(context.Background(), level, strings.TrimSpace(line))
		return
	}
	var attrs []any
	for k, v := range fields {
		if k == "msg" || k == "level" || k == "time" {
			continue
		}
		attrs = append(attrs, k, v)
	}
	w.log.Log(context.Background(), level, msg, attrs...)
}

// parseLogfmt parses key=value pairs where values may be double quoted.
func parseLogfmt(line string) map[string]string {
	out := map[string]string{}
	s := strings.TrimSpace(line)
	for s != "" {
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || strings.ContainsAny(s[:eq], " \t") {
			break
		}
		key := s[:eq]
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := 1
			for end < len(s) {
				if s[end] == '\\' {
					end += 2
					continue
				}
				if s[end] == '"' {
					break
				}
				end++
			}
			if end >= len(s) {
				end = len(s) - 1
			}
			q := s[:end+1]
			if u, err := strconv.Unquote(q); err == nil {
				val = u
			} else {
				val = strings.Trim(q, `"`)
			}
			s = s[min(end+1, len(s)):]
		} else {
			sp := strings.IndexAny(s, " \t")
			if sp < 0 {
				sp = len(s)
			}
			val = s[:sp]
			s = s[sp:]
		}
		out[key] = val
		s = strings.TrimLeft(s, " \t")
	}
	return out
}
