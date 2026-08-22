// Package binding decodes request bodies into Go structs.
//
// It dispatches on the request Content-Type: JSON and XML bodies are decoded
// whole, while urlencoded and multipart bodies are mapped field by field using
// the `form` struct tags. Field level conversion failures do not abort the
// decode: every remaining field is still populated and the collected failures
// are returned as a single joined error, which lets callers decide whether a
// partially decoded form is fatal.
//
// The form decoder understands hdur.Duration values such as "24h", which the
// standard binders cannot parse because hdur.Duration only implements
// json.Unmarshaler.
package binding

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/watzon/hdur"
)

// defaultMultipartMemory mirrors net/http's own multipart memory budget.
const defaultMultipartMemory = 32 << 20 // 32 MB

// ErrUnsupportedMediaType is returned when the request Content-Type cannot be
// decoded into a struct.
var ErrUnsupportedMediaType = errors.New("unprocessable entity: unsupported content type")

// Body decodes the body of the current request into out, which must be a
// non-nil pointer to a struct.
func Body(c *gin.Context, out any) error {
	switch contentType := strings.ToLower(c.ContentType()); {
	case contentType == "application/json" || strings.HasSuffix(contentType, "+json"):
		body, err := ReadBody(c)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)

	case contentType == "application/xml" || contentType == "text/xml" || strings.HasSuffix(contentType, "+xml"):
		body, err := ReadBody(c)
		if err != nil {
			return err
		}
		return xml.Unmarshal(body, out)

	case contentType == "application/x-www-form-urlencoded":
		if err := c.Request.ParseForm(); err != nil {
			return err
		}
		return decodeForm(c.Request.PostForm, out)

	case contentType == "multipart/form-data":
		if err := c.Request.ParseMultipartForm(defaultMultipartMemory); err != nil {
			return err
		}
		if c.Request.MultipartForm == nil {
			return errors.New("binding: multipart form is empty")
		}
		return decodeForm(c.Request.MultipartForm.Value, out)
	}

	return ErrUnsupportedMediaType
}

// ReadBody returns the raw request body and restores it so that later readers
// still see the full payload.
func ReadBody(c *gin.Context) ([]byte, error) {
	if c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))

	return body, nil
}

// decodeForm maps form values onto the `form` tagged fields of out. Missing and
// empty values leave the field at its zero value.
func decodeForm(values map[string][]string, out any) error {
	target := reflect.ValueOf(out)
	for target.Kind() == reflect.Ptr {
		if target.IsNil() {
			return errors.New("binding: target must be a non-nil pointer")
		}
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct {
		return errors.New("binding: target must point to a struct")
	}

	var failures []error
	targetType := target.Type()
	for i := range targetType.NumField() {
		field := target.Field(i)
		if !field.CanSet() {
			continue
		}

		name := fieldName(targetType.Field(i))
		if name == "" {
			continue
		}

		raw, ok := values[name]
		if !ok || len(raw) == 0 || raw[0] == "" {
			continue
		}

		if err := setValue(field, raw[0]); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
	}

	return errors.Join(failures...)
}

// fieldName resolves the form key for a struct field, returning "" when the
// field is explicitly excluded.
func fieldName(field reflect.StructField) string {
	name, ok := field.Tag.Lookup("form")
	if !ok {
		return field.Name
	}
	if name, _, _ = strings.Cut(name, ","); name == "-" {
		return ""
	}
	if name == "" {
		return field.Name
	}
	return name
}

// setValue converts raw into the type of field. A pointer field is left nil
// when the conversion fails, matching the "zero on error" behaviour the
// handlers rely on.
func setValue(field reflect.Value, raw string) error {
	if field.Kind() == reflect.Ptr {
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		if err := setValue(field.Elem(), raw); err != nil {
			field.Set(reflect.Zero(field.Type()))
			return err
		}
		return nil
	}

	switch field.Interface().(type) {
	case hdur.Duration:
		duration, err := hdur.ParseDuration(raw)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(duration))
		return nil
	case time.Time:
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return err
		}
		field.Set(reflect.ValueOf(parsed))
		return nil
	}

	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Bool:
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		field.SetBool(value)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(raw, 10, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetInt(value)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := strconv.ParseUint(raw, 10, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetUint(value)
	case reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(raw, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetFloat(value)
	case reflect.Struct, reflect.Slice, reflect.Map:
		return json.Unmarshal([]byte(raw), field.Addr().Interface())
	default:
		return fmt.Errorf("binding: unsupported field type %s", field.Type())
	}

	return nil
}
