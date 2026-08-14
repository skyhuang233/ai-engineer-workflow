package setupcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"
)

// Canonicalize applies the Setup Contract's restricted JSON canonicalization:
// valid UTF-8, objects with unique code-point-sorted keys, unescaped Unicode,
// minimal JSON string escapes, and arbitrary-size base-10 integers only.
func Canonicalize(raw []byte) ([]byte, string, error) {
	if !utf8.Valid(raw) {
		return nil, "", errors.New("canonical JSON must be valid UTF-8")
	}
	if err := validateUnicodeEscapes(raw); err != nil {
		return nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeRestricted(decoder)
	if err != nil {
		return nil, "", err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, "", fmt.Errorf("canonical JSON has trailing token %v", token)
		}
		return nil, "", fmt.Errorf("decode trailing canonical JSON: %w", err)
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(out.Bytes())
	return out.Bytes(), hex.EncodeToString(sum[:]), nil
}

func decodeRestricted(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode canonical JSON: %w", err)
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, fmt.Errorf("decode object key: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok || !validUnicodeString(key) {
					return nil, errors.New("object key must be valid Unicode")
				}
				if _, duplicate := object[key]; duplicate {
					return nil, fmt.Errorf("duplicate object field %q", key)
				}
				child, err := decodeRestricted(decoder)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, errors.New("unterminated JSON object")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				child, err := decodeRestricted(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, child)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return nil, errors.New("unterminated JSON array")
			}
			return array, nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		text := string(value)
		if strings.ContainsAny(text, ".eE") {
			return nil, errors.New("floating-point values are forbidden")
		}
		integer := new(big.Int)
		if _, ok := integer.SetString(text, 10); !ok {
			return nil, fmt.Errorf("invalid integer %q", text)
		}
		return integer, nil
	case string:
		if !validUnicodeString(value) {
			return nil, errors.New("string must contain valid Unicode scalar values")
		}
		return value, nil
	case bool, nil:
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", token)
	}
}

func validUnicodeString(value string) bool {
	return utf8.ValidString(value)
}

func validateUnicodeEscapes(raw []byte) error {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(raw) {
				continue
			}
			index++
			if raw[index] != 'u' {
				continue
			}
			value, ok := unicodeEscapeValue(raw, index+1)
			if !ok {
				return errors.New("invalid Unicode escape")
			}
			index += 4
			if value >= 0xd800 && value <= 0xdbff {
				if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
					return errors.New("unpaired high-surrogate Unicode escape")
				}
				low, ok := unicodeEscapeValue(raw, index+3)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return errors.New("unpaired high-surrogate Unicode escape")
				}
				index += 6
			} else if value >= 0xdc00 && value <= 0xdfff {
				return errors.New("unpaired low-surrogate Unicode escape")
			}
		}
	}
	return nil
}

func unicodeEscapeValue(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, character := range raw[start : start+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value += uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value += uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value += uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func writeCanonical(out *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case *big.Int:
		out.WriteString(typed.String())
	case string:
		writeJSONString(out, typed)
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeJSONString(out, key)
			out.WriteByte(':')
			if err := writeCanonical(out, typed[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical value %T", value)
	}
	return nil
}

func writeJSONString(out *bytes.Buffer, value string) {
	const hexDigits = "0123456789abcdef"
	out.WriteByte('"')
	for _, runeValue := range value {
		switch runeValue {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(runeValue)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if runeValue < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[byte(runeValue)>>4])
				out.WriteByte(hexDigits[byte(runeValue)&0x0f])
			} else {
				out.WriteRune(runeValue)
			}
		}
	}
	out.WriteByte('"')
}
