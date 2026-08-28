package outbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// CanonicalizeJSON converts one JSON document into its RFC 8785 (JSON
// Canonicalization Scheme) form: object members sorted lexicographically, no
// insignificant whitespace, minimal string escaping and ECMAScript number
// serialization. It is the signing input encoding for envelope provenance
// signatures, so any deviation is a signature break — never a silent reformat.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON for canonicalization: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("input must contain exactly one JSON document")
	}
	canonical, err := Canonicalize(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

// Canonicalize serializes one decoded JSON value (maps, slices, strings,
// json.Number, bool, nil) to RFC 8785 form. Values produced by an
// encoding/json decoder with UseNumber are the supported input.
func Canonicalize(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeCanonicalValue(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeCanonicalValue(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		writeCanonicalString(buffer, typed)
	case json.Number:
		canonical, err := canonicalNumber(typed.String())
		if err != nil {
			return err
		}
		buffer.WriteString(canonical)
	case float64:
		buffer.WriteString(ecmaScriptNumber(typed))
	case []any:
		buffer.WriteByte('[')
		for index, element := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeCanonicalValue(buffer, element); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			writeCanonicalString(buffer, key)
			buffer.WriteByte(':')
			if err := writeCanonicalValue(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return fmt.Errorf("value of type %T is not JSON-canonicalizable", value)
	}
	return nil
}

// writeCanonicalString emits a JSON string with the minimal RFC 8785 escape
// set: only the two-letter control escapes, quote, backslash and \u00XX for
// the remaining C0 controls. All other characters are literal UTF-8.
func writeCanonicalString(buffer *bytes.Buffer, value string) {
	const hexDigits = "0123456789abcdef"
	buffer.WriteByte('"')
	for index := 0; index < len(value); index++ {
		switch octet := value[index]; octet {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if octet < 0x20 {
				buffer.WriteString(`\u00`)
				buffer.WriteByte(hexDigits[octet>>4])
				buffer.WriteByte(hexDigits[octet&0x0f])
			} else {
				buffer.WriteByte(octet)
			}
		}
	}
	buffer.WriteByte('"')
}

// maxExactInteger is the largest magnitude JSON integer that an IEEE-754
// double (and therefore ECMAScript JSON.stringify) represents exactly.
var maxExactInteger = new(big.Int).Lsh(big.NewInt(1), 53)

// canonicalNumber renders one JSON number literal per RFC 8785: integers
// within the exactly-representable range keep their digits; everything else
// goes through IEEE-754 double conversion and ECMAScript serialization.
func canonicalNumber(literal string) (string, error) {
	if literal == "" {
		return "", errors.New("empty JSON number")
	}
	if !strings.ContainsAny(literal, ".eE") {
		integer, ok := new(big.Int).SetString(literal, 10)
		if !ok {
			return "", fmt.Errorf("invalid JSON number %q", literal)
		}
		if new(big.Int).Abs(integer).Cmp(maxExactInteger) <= 0 {
			return integer.String(), nil
		}
	}
	parsed, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return "", fmt.Errorf("invalid JSON number %q", literal)
	}
	return ecmaScriptNumber(parsed), nil
}

// ecmaScriptNumber serializes one IEEE-754 double exactly as ECMAScript
// Number::toString / JSON.stringify do, which RFC 8785 mandates: plain
// decimal for 1e-6 <= |v| < 1e21, exponential otherwise.
func ecmaScriptNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	// Shortest round-trip digits in scientific form: "d[.ddd]e<exp>".
	scientific := strconv.FormatFloat(value, 'e', -1, 64)
	eIndex := strings.IndexByte(scientific, 'e')
	mantissa := scientific[:eIndex]
	exponent, err := strconv.Atoi(scientific[eIndex+1:])
	if err != nil {
		panic(fmt.Sprintf("unexpected Go scientific format %q", scientific))
	}
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	n := exponent + 1 // value = 0.d1d2...dk * 10^n with d1 != 0
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	default:
		exponentSign := "+"
		if n-1 < 0 {
			exponentSign = "-"
		}
		exponentDigits := strconv.Itoa(abs(n - 1))
		if k == 1 {
			return sign + digits + "e" + exponentSign + exponentDigits
		}
		return sign + digits[:1] + "." + digits[1:] + "e" + exponentSign + exponentDigits
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
