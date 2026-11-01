package ipp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	m := NewRequest(OpPrintJob, 7)
	m.Add(TagOperation, String(TagURI, "printer-uri", "ipp://p/ipp/print"), Keyword("requested-attributes", "a", "b"))
	m.Add(TagJob, Integer("copies", 3), Enum("print-quality", 5), Boolean("ipp-attribute-fidelity", true))
	data := append(m.Encode(), "document"...)
	r := bufio.NewReader(bytes.NewReader(data))
	got, err := Decode(r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != OpPrintJob || got.RequestID != 7 || got.Major != 2 || len(got.Groups) != 2 {
		t.Fatalf("message %+v", got)
	}
	if v := got.Attr(TagOperation, "requested-attributes").Strings(); len(v) != 2 || v[1] != "b" {
		t.Fatalf("multi-value %v", v)
	}
	if n, _ := got.Attr(TagJob, "copies").Int(); n != 3 {
		t.Fatal(n)
	}
	if b, ok := got.Attr(TagJob, "ipp-attribute-fidelity").Bool(); !b || !ok {
		t.Fatal("bool")
	}
	if rest, _ := r.ReadString(0); rest != "document" {
		t.Fatalf("document %q", rest)
	}
}

func TestDecodeSkipsCollections(t *testing.T) {
	text := []byte{0, 2, 'e', 'n', 0, 5, 'H', 'e', 'l', 'l', 'o'}
	m := &Message{Major: 2, Code: StatusOK, RequestID: 1}
	m.Add(TagPrinter,
		Attribute{Name: "media-col-database", Values: []Value{
			{Tag: TagBeginCollection},
			{Tag: TagMemberName, Data: []byte("media-size")},
			{Tag: TagBeginCollection},
			{Tag: TagMemberName, Data: []byte("x-dimension")},
			{Tag: TagInteger, Data: binary.BigEndian.AppendUint32(nil, 21000)},
			{Tag: TagEndCollection},
			{Tag: TagEndCollection},
			{Tag: TagBeginCollection},
			{Tag: TagEndCollection},
		}},
		Attribute{Name: "printer-info", Values: []Value{{Tag: TagTextLang, Data: text}}},
		Attribute{Name: "printer-resolution-supported", Values: []Value{
			{Tag: TagResolution, Data: append(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 600), 600), 3)},
			{Tag: TagResolution, Data: append(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 118), 118), 4)},
		}})
	got, err := Decode(bytes.NewReader(m.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Attr(TagPrinter, "media-col-database").Values; len(v) != 2 || v[0].Tag != TagBeginCollection {
		t.Fatalf("collections %+v", v)
	}
	if s := got.Attr(TagPrinter, "printer-info").String(); s != "Hello" {
		t.Fatalf("text with language %q", s)
	}
	if r := got.Attr(TagPrinter, "printer-resolution-supported").Resolutions(); len(r) != 2 || r[0].X != 600 || r[1].X != 300 {
		t.Fatalf("resolutions %v", r)
	}
	if _, err := Decode(bytes.NewReader(m.Encode()[:20])); err == nil {
		t.Fatal("truncated message decoded")
	}
}

func TestHTTPURL(t *testing.T) {
	for in, want := range map[string]string{
		"ipp://printer/ipp/print":      "http://printer:631/ipp/print",
		"ipps://printer:443/ipp/print": "https://printer:443/ipp/print",
		"ipp://192.168.1.5:8631/x":     "http://192.168.1.5:8631/x",
	} {
		if got, err := HTTPURL(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := HTTPURL("socket://printer:9100"); err == nil {
		t.Error("socket scheme accepted")
	}
}
