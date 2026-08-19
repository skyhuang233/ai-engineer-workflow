package workflowrelease

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

func decodeStrict(raw []byte, target any) error {
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return err
	}
	if err := rejectNonExactJSONFields(raw, target); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains more than one value")
		}
		return err
	}
	return nil
}

func rejectNonExactJSONFields(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return validateExactJSONFields(value, reflect.TypeOf(target), "$")
}

var rawMessageType = reflect.TypeOf(json.RawMessage{})

func validateExactJSONFields(value any, target reflect.Type, path string) error {
	for target.Kind() == reflect.Ptr {
		target = target.Elem()
	}
	if target == rawMessageType {
		return nil
	}
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil // The typed decoder reports the value-kind error.
		}
		fields := make(map[string]reflect.Type, target.NumField())
		for index := 0; index < target.NumField(); index++ {
			field := target.Field(index)
			if field.PkgPath != "" {
				continue
			}
			name := field.Name
			if tag := field.Tag.Get("json"); tag != "" {
				name = strings.Split(tag, ",")[0]
			}
			if name != "-" && name != "" {
				fields[name] = field.Type
			}
		}
		for name, nested := range object {
			fieldType, exists := fields[name]
			if !exists {
				return fmt.Errorf("%s contains unknown or incorrectly cased field %q", path, name)
			}
			if err := validateExactJSONFields(nested, fieldType, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		for index, item := range items {
			if err := validateExactJSONFields(item, target.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case reflect.Map, reflect.Interface:
		return nil
	}
	return nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := nameToken.(string)
				if !ok {
					return errors.New("JSON object member name is not a string")
				}
				if _, duplicate := seen[name]; duplicate {
					return fmt.Errorf("duplicate JSON field %q", name)
				}
				seen[name] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains more than one value")
		}
		return err
	}
	return nil
}
