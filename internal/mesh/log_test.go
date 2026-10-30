package mesh

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLogWriter(t *testing.T) {
	var buf bytes.Buffer
	w := newLogWriter(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	w.Write([]byte(`time="2026-01-01T00:00:00Z" level=warning msg="peer \"x\" disconnected" peer=abc` + "\n" + `level=debu`))
	w.Write([]byte("g msg=started\nplain text line\n"))
	out := buf.String()
	for _, want := range []string{`level=WARN msg="peer \"x\" disconnected" peer=abc`, `level=DEBUG msg=started`, `level=INFO msg="plain text line"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestParseLogfmt(t *testing.T) {
	f := parseLogfmt(`level=info msg="a b" empty="" k=v`)
	if f["level"] != "info" || f["msg"] != "a b" || f["empty"] != "" || f["k"] != "v" {
		t.Fatalf("%v", f)
	}
}

func TestLogWriterStopping(t *testing.T) {
	var buf bytes.Buffer
	w := newLogWriter(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	w.stopping.Store(true)
	w.Write([]byte("level=error msg=\"context canceled\"\n"))
	if buf.Len() != 0 {
		t.Fatalf("error during shutdown not demoted: %s", buf.String())
	}
}
