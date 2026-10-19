package parity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// ErrFloat is returned when a payload contains a non-integer JSON number
// (any number written with a fraction or an exponent). Hashed payloads carry
// integers only; prices are integer micro-USD per 1M tokens.
var ErrFloat = errors.New("float in canonical payload")

// Canonical marshals v and returns its canonical JSON bytes. A float64 field
// that happens to hold a whole number marshals as an integer literal and is not
// detected here, so hashed types must not declare float fields at all.
func Canonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("canonical: marshal: %w", err)
	}
	return CanonicalJSON(buf.Bytes())
}

// CanonicalJSON re-encodes one JSON document so that it is byte-identical to
//
//	json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
//
// Numbers are decoded as json.Number, never float64, so integers of any size
// survive. A number with a fraction or an exponent is rejected with ErrFloat.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("canonical: decode: trailing data after document")
	}
	var out bytes.Buffer
	if err := write(&out, v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// SHA256Hex returns the lowercase hex sha256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Hash returns the sha256 of the canonical encoding of v.
func Hash(v any) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	return SHA256Hex(b), nil
}

func write(out *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if x {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		writeString(out, x)
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return fmt.Errorf("%w: %s", ErrFloat, s)
		}
		if s == "-0" {
			s = "0" // Python loads -0 as the int 0
		}
		out.WriteString(s)
	case []any:
		out.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := write(out, e); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys) // byte order of valid UTF-8 equals codepoint order
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, k)
			out.WriteByte(':')
			if err := write(out, x[k]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("canonical: unexpected %T", v)
	}
	return nil
}

const hexDigits = "0123456789abcdef"

// writeString follows Python's json encoder with ensure_ascii=False: only the
// quote, the backslash and control characters below 0x20 are escaped. Go's own
// encoder also escapes U+2028 and U+2029, which Python does not.
func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			out.WriteString(`\"`)
		case r == '\\':
			out.WriteString(`\\`)
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\b':
			out.WriteString(`\b`)
		case r == '\f':
			out.WriteString(`\f`)
		case r < 0x20:
			out.WriteString(`\u00`)
			out.WriteByte(hexDigits[r>>4])
			out.WriteByte(hexDigits[r&0xf])
		case r == utf8.RuneError:
			out.WriteString("\uFFFD")
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
}
