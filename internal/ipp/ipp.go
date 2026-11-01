// Package ipp encodes and decodes Internet Printing Protocol messages
// (RFC 8010, RFC 8011) and sends them over HTTP. It covers what platen
// needs to print on driverless printers (IPP Everywhere, AirPrint):
// Get-Printer-Attributes, Print-Job and Get-Job-Attributes.
package ipp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Operations.
const (
	OpPrintJob             uint16 = 0x0002
	OpValidateJob          uint16 = 0x0004
	OpCancelJob            uint16 = 0x0008
	OpGetJobAttributes     uint16 = 0x0009
	OpGetJobs              uint16 = 0x000A
	OpGetPrinterAttributes uint16 = 0x000B
)

// Delimiter tags start an attribute group.
const (
	TagOperation   byte = 0x01
	TagJob         byte = 0x02
	TagEnd         byte = 0x03
	TagPrinter     byte = 0x04
	TagUnsupported byte = 0x05
)

// Value tags.
const (
	TagUnsupportedValue byte = 0x10
	TagUnknown          byte = 0x12
	TagNoValue          byte = 0x13
	TagInteger          byte = 0x21
	TagBoolean          byte = 0x22
	TagEnum             byte = 0x23
	TagOctetString      byte = 0x30
	TagDateTime         byte = 0x31
	TagResolution       byte = 0x32
	TagRange            byte = 0x33
	TagBeginCollection  byte = 0x34
	TagTextLang         byte = 0x35
	TagNameLang         byte = 0x36
	TagEndCollection    byte = 0x37
	TagText             byte = 0x41
	TagName             byte = 0x42
	TagKeyword          byte = 0x44
	TagURI              byte = 0x45
	TagURIScheme        byte = 0x46
	TagCharset          byte = 0x47
	TagLanguage         byte = 0x48
	TagMimeType         byte = 0x49
	TagMemberName       byte = 0x4A
)

// Job states (job-state).
const (
	JobPending    = 3
	JobHeld       = 4
	JobProcessing = 5
	JobStopped    = 6
	JobCanceled   = 7
	JobAborted    = 8
	JobCompleted  = 9
)

// Status codes.
const (
	StatusOK                         uint16 = 0x0000
	StatusBadRequest                 uint16 = 0x0400
	StatusNotFound                   uint16 = 0x0406
	StatusDocumentFormatNotSupported uint16 = 0x040A
	StatusInternalError              uint16 = 0x0500
	StatusServiceUnavailable         uint16 = 0x0502
	StatusVersionNotSupported        uint16 = 0x0503
	StatusDeviceError                uint16 = 0x0504
	StatusTemporaryError             uint16 = 0x0505
	StatusNotAcceptingJobs           uint16 = 0x0506
	StatusBusy                       uint16 = 0x0507
)

// Value is one value of an attribute in its encoded form.
type Value struct {
	Tag  byte
	Data []byte
}

// Attribute is a named attribute with one or more values.
type Attribute struct {
	Name   string
	Values []Value
}

// Group is an attribute group, e.g. the operation or printer attributes.
type Group struct {
	Tag   byte
	Attrs []Attribute
}

// Message is an IPP request or response. Code is the operation of a
// request and the status of a response.
type Message struct {
	Major, Minor byte
	Code         uint16
	RequestID    uint32
	Groups       []Group
}

// NewRequest returns an IPP/2.0 request with the operation attributes
// every request starts with.
func NewRequest(op uint16, id uint32) *Message {
	m := &Message{Major: 2, Minor: 0, Code: op, RequestID: id}
	m.Add(TagOperation,
		String(TagCharset, "attributes-charset", "utf-8"),
		String(TagLanguage, "attributes-natural-language", "en"))
	return m
}

// Add appends attributes to the last group with the tag, or to a new
// group.
func (m *Message) Add(group byte, attrs ...Attribute) {
	if n := len(m.Groups); n > 0 && m.Groups[n-1].Tag == group {
		m.Groups[n-1].Attrs = append(m.Groups[n-1].Attrs, attrs...)
		return
	}
	m.Groups = append(m.Groups, Group{Tag: group, Attrs: attrs})
}

// Attr returns the first attribute with the name in a group with the tag,
// or nil.
func (m *Message) Attr(group byte, name string) *Attribute {
	for gi := range m.Groups {
		if m.Groups[gi].Tag != group {
			continue
		}
		for ai := range m.Groups[gi].Attrs {
			if m.Groups[gi].Attrs[ai].Name == name {
				return &m.Groups[gi].Attrs[ai]
			}
		}
	}
	return nil
}

// Success reports whether the status of a response is successful.
func (m *Message) Success() bool { return m.Code <= 0x00ff }

// Err returns a *StatusError for an unsuccessful response.
func (m *Message) Err() error {
	if m.Success() {
		return nil
	}
	e := &StatusError{Code: m.Code}
	if a := m.Attr(TagOperation, "status-message"); a != nil {
		e.Message = a.String()
	}
	return e
}

// StatusError is an unsuccessful IPP response.
type StatusError struct {
	Code    uint16
	Message string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("ipp: %s (0x%04x)", statusName(e.Code), e.Code)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func statusName(code uint16) string {
	switch code {
	case StatusBadRequest:
		return "client-error-bad-request"
	case 0x0401:
		return "client-error-forbidden"
	case 0x0402:
		return "client-error-not-authenticated"
	case 0x0403:
		return "client-error-not-authorized"
	case 0x0404:
		return "client-error-not-possible"
	case 0x0405:
		return "client-error-timeout"
	case StatusNotFound:
		return "client-error-not-found"
	case 0x0408:
		return "client-error-request-entity-too-large"
	case StatusDocumentFormatNotSupported:
		return "client-error-document-format-not-supported"
	case 0x040B:
		return "client-error-attributes-or-values-not-supported"
	case StatusInternalError:
		return "server-error-internal-error"
	case 0x0501:
		return "server-error-operation-not-supported"
	case StatusServiceUnavailable:
		return "server-error-service-unavailable"
	case StatusVersionNotSupported:
		return "server-error-version-not-supported"
	case StatusDeviceError:
		return "server-error-device-error"
	case StatusTemporaryError:
		return "server-error-temporary-error"
	case StatusNotAcceptingJobs:
		return "server-error-not-accepting-jobs"
	case StatusBusy:
		return "server-error-busy"
	}
	if code >= 0x0400 && code < 0x0500 {
		return "client-error"
	}
	return "server-error"
}

// ---- attribute constructors ----

// String returns an attribute with string values such as keyword, name,
// text, uri or mimeMediaType.
func String(tag byte, name string, values ...string) Attribute {
	a := Attribute{Name: name}
	for _, v := range values {
		a.Values = append(a.Values, Value{Tag: tag, Data: []byte(v)})
	}
	return a
}

// Keyword returns a keyword attribute.
func Keyword(name string, values ...string) Attribute { return String(TagKeyword, name, values...) }

// Integer returns an integer attribute.
func Integer(name string, values ...int) Attribute { return ints(TagInteger, name, values) }

// Enum returns an enum attribute.
func Enum(name string, values ...int) Attribute { return ints(TagEnum, name, values) }

func ints(tag byte, name string, values []int) Attribute {
	a := Attribute{Name: name}
	for _, v := range values {
		a.Values = append(a.Values, Value{Tag: tag, Data: binary.BigEndian.AppendUint32(nil, uint32(int32(v)))})
	}
	return a
}

// Boolean returns a boolean attribute.
func Boolean(name string, v bool) Attribute {
	b := byte(0)
	if v {
		b = 1
	}
	return Attribute{Name: name, Values: []Value{{Tag: TagBoolean, Data: []byte{b}}}}
}

// ---- attribute accessors ----

func isString(tag byte) bool {
	switch tag {
	case TagText, TagName, TagKeyword, TagURI, TagURIScheme, TagCharset, TagLanguage, TagMimeType:
		return true
	}
	return false
}

// Strings returns the string values of the attribute.
func (a *Attribute) Strings() []string {
	if a == nil {
		return nil
	}
	var out []string
	for _, v := range a.Values {
		switch {
		case isString(v.Tag):
			out = append(out, string(v.Data))
		case (v.Tag == TagTextLang || v.Tag == TagNameLang) && len(v.Data) >= 2:
			// language length, language, text length, text
			n := int(binary.BigEndian.Uint16(v.Data))
			if len(v.Data) >= 4+n {
				out = append(out, string(v.Data[4+n:]))
			}
		}
	}
	return out
}

// String returns the first string value, or "".
func (a *Attribute) String() string {
	if s := a.Strings(); len(s) > 0 {
		return s[0]
	}
	return ""
}

// Has reports whether a string value equals s.
func (a *Attribute) Has(s string) bool {
	for _, v := range a.Strings() {
		if v == s {
			return true
		}
	}
	return false
}

// Ints returns the integer and enum values of the attribute.
func (a *Attribute) Ints() []int {
	if a == nil {
		return nil
	}
	var out []int
	for _, v := range a.Values {
		if (v.Tag == TagInteger || v.Tag == TagEnum) && len(v.Data) == 4 {
			out = append(out, int(int32(binary.BigEndian.Uint32(v.Data))))
		}
	}
	return out
}

// Int returns the first integer value and whether there is one.
func (a *Attribute) Int() (int, bool) {
	if v := a.Ints(); len(v) > 0 {
		return v[0], true
	}
	return 0, false
}

// Bool returns the first boolean value and whether there is one.
func (a *Attribute) Bool() (bool, bool) {
	if a == nil {
		return false, false
	}
	for _, v := range a.Values {
		if v.Tag == TagBoolean && len(v.Data) == 1 {
			return v.Data[0] != 0, true
		}
	}
	return false, false
}

// Range is a rangeOfInteger value.
type Range struct{ Lower, Upper int }

// Ranges returns the rangeOfInteger values; plain integers become ranges
// of one value.
func (a *Attribute) Ranges() []Range {
	if a == nil {
		return nil
	}
	var out []Range
	for _, v := range a.Values {
		switch {
		case v.Tag == TagRange && len(v.Data) == 8:
			out = append(out, Range{int(int32(binary.BigEndian.Uint32(v.Data))), int(int32(binary.BigEndian.Uint32(v.Data[4:])))})
		case v.Tag == TagInteger && len(v.Data) == 4:
			n := int(int32(binary.BigEndian.Uint32(v.Data)))
			out = append(out, Range{n, n})
		}
	}
	return out
}

// Resolution is a resolution value in dots per inch.
type Resolution struct{ X, Y int }

// Resolutions returns the resolution values in dots per inch.
func (a *Attribute) Resolutions() []Resolution {
	if a == nil {
		return nil
	}
	var out []Resolution
	for _, v := range a.Values {
		if v.Tag != TagResolution || len(v.Data) != 9 {
			continue
		}
		x, y := int(int32(binary.BigEndian.Uint32(v.Data))), int(int32(binary.BigEndian.Uint32(v.Data[4:])))
		if v.Data[8] == 4 { // dots per centimeter
			x, y = (x*254+50)/100, (y*254+50)/100
		}
		out = append(out, Resolution{x, y})
	}
	return out
}

// ---- encoding ----

// Encode returns the message in its wire format.
func (m *Message) Encode() []byte {
	var b bytes.Buffer
	b.Write([]byte{m.Major, m.Minor})
	b.Write(binary.BigEndian.AppendUint16(nil, m.Code))
	b.Write(binary.BigEndian.AppendUint32(nil, m.RequestID))
	for _, g := range m.Groups {
		b.WriteByte(g.Tag)
		for _, a := range g.Attrs {
			for i, v := range a.Values {
				name := a.Name
				if i > 0 {
					name = "" // additional value of the same attribute
				}
				b.WriteByte(v.Tag)
				b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(name))))
				b.WriteString(name)
				b.Write(binary.BigEndian.AppendUint16(nil, uint16(len(v.Data))))
				b.Write(v.Data)
			}
		}
	}
	b.WriteByte(TagEnd)
	return b.Bytes()
}

// Decode reads a message up to and including the end-of-attributes tag.
// Data after it (the document of a request) is left unread in r, so r
// should be buffered. Collection values are kept as one placeholder value
// with tag TagBeginCollection; their members are skipped.
func Decode(r io.Reader) (*Message, error) {
	br, ok := r.(io.ByteReader)
	if !ok {
		b := bufio.NewReader(r)
		r, br = b, b
	}
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("ipp: read header: %w", err)
	}
	m := &Message{Major: head[0], Minor: head[1], Code: binary.BigEndian.Uint16(head[2:]), RequestID: binary.BigEndian.Uint32(head[4:])}
	var group *Group
	var attr *Attribute
	depth := 0
	readField := func() ([]byte, error) {
		var n [2]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return nil, err
		}
		buf := make([]byte, binary.BigEndian.Uint16(n[:]))
		_, err := io.ReadFull(r, buf)
		return buf, err
	}
	for {
		tag, err := br.ReadByte()
		if err != nil {
			return nil, errors.New("ipp: truncated message")
		}
		if tag <= 0x0f && depth == 0 {
			if tag == TagEnd {
				return m, nil
			}
			m.Groups = append(m.Groups, Group{Tag: tag})
			group, attr = &m.Groups[len(m.Groups)-1], nil
			continue
		}
		name, err := readField()
		if err != nil {
			return nil, errors.New("ipp: truncated attribute")
		}
		value, err := readField()
		if err != nil {
			return nil, errors.New("ipp: truncated attribute")
		}
		if group == nil {
			return nil, errors.New("ipp: attribute outside a group")
		}
		switch {
		case depth > 0:
			// Inside a collection: skip members, track nesting.
			switch tag {
			case TagBeginCollection:
				depth++
			case TagEndCollection:
				depth--
			}
			continue
		case len(name) > 0:
			group.Attrs = append(group.Attrs, Attribute{Name: string(name)})
			attr = &group.Attrs[len(group.Attrs)-1]
		case attr == nil:
			return nil, errors.New("ipp: additional value without attribute")
		}
		if tag == TagBeginCollection {
			depth = 1
			value = nil
		}
		attr.Values = append(attr.Values, Value{Tag: tag, Data: value})
	}
}
