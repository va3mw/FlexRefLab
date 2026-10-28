// Package safejson converts values for JSON where NaN and ±Inf (used for
// "no reading") become null; encoding/json refuses them outright.
package safejson

import (
	"encoding"
	"math"
	"reflect"
	"strings"
	"time"
)

// Tree returns v as nested maps, slices and scalars, honouring json tags.
func Tree(v any) any { return conv(reflect.ValueOf(v)) }

var timeType = reflect.TypeOf(time.Time{})

func conv(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	if v.Type() == timeType {
		t := v.Interface().(time.Time)
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return conv(v.Elem())
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return f
	case reflect.Slice:
		if v.IsNil() {
			return []any{}
		}
		fallthrough
	case reflect.Array:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = conv(v.Index(i))
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		it := v.MapRange()
		for it.Next() {
			k := it.Key()
			var ks string
			if tm, ok := k.Interface().(encoding.TextMarshaler); ok {
				b, _ := tm.MarshalText()
				ks = string(b)
			} else {
				ks = k.String()
			}
			out[ks] = conv(it.Value())
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if n, _, _ := strings.Cut(tag, ","); n != "" {
				name = n
			}
			out[name] = conv(v.Field(i))
		}
		return out
	}
	return v.Interface()
}
