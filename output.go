package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"

	toon "github.com/toon-format/toon-go"
)

// emitStructured writes v to stdout in the requested format. Format must be
// "json" or "toon"; any other value is a programming error.
func emitStructured(format string, v any) error {
	if format == "toon" {
		v = scrubControlChars(v)
	}
	return writeStructured(os.Stdout, format, v)
}

// scrubControlChars walks v and returns a value with raw control characters
// replaced by spaces in any string field. toon-go rejects bytes in
// 0x00–0x1F (except \t \n \r) — they appear in transcripts when agents capture
// terminal output with ANSI escapes — and JSON is unaffected so we only run
// this on the TOON path. Use reflection rather than per-shape helpers so new
// payload structs work without code changes.
func scrubControlChars(v any) any {
	rv := reflect.ValueOf(v)
	out := scrubReflect(rv)
	if !out.IsValid() {
		return v
	}
	return out.Interface()
}

func scrubReflect(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		cleaned := scrubControlString(s)
		if cleaned == s {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.SetString(cleaned)
		return out
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return v
		}
		inner := scrubReflect(v.Elem())
		if !inner.IsValid() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		if v.Kind() == reflect.Interface {
			out.Set(inner)
		} else {
			ptr := reflect.New(inner.Type())
			ptr.Elem().Set(inner)
			out.Set(ptr)
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(scrubReflect(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(scrubReflect(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			f := out.Field(i)
			if !f.CanSet() {
				continue
			}
			f.Set(scrubReflect(v.Field(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), scrubReflect(iter.Value()))
		}
		return out
	}
	return v
}

// scrubControlString replaces control characters (other than \t \n \r) with a
// single space. Returns the original string when no replacement is needed so
// callers can detect a no-op cheaply.
func scrubControlString(s string) string {
	needsScrub := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
			needsScrub = true
			break
		}
	}
	if !needsScrub {
		return s
	}
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
			b[i] = ' '
			continue
		}
		b[i] = c
	}
	return string(b)
}

func writeStructured(w io.Writer, format string, v any) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "toon":
		b, err := toon.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		// toon.Marshal does not always trail a newline; keep terminals happy.
		if len(b) == 0 || b[len(b)-1] != '\n' {
			_, err = w.Write([]byte{'\n'})
		}
		return err
	}
	return fmt.Errorf("emitStructured: unsupported format %q", format)
}

// resolveFields normalises a --fields spec against a command's allowed and
// default field sets. Empty input returns the trimmed defaults (the reason
// this exists — agents shouldn't pay tokens for fields they almost never
// read). "all" returns every allowed field. Anything else must be a subset of
// allowed; an unknown field is an error so typos don't silently drop data.
func resolveFields(spec string, defaults, allowed []string) ([]string, error) {
	raw := strings.TrimSpace(spec)
	if raw == "" {
		return defaults, nil
	}
	if strings.EqualFold(raw, "all") {
		return allowed, nil
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = true
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !allowedSet[p] {
			return nil, fmt.Errorf("unknown field %q (allowed: %s)", p, strings.Join(allowed, ", "))
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return defaults, nil
	}
	return out, nil
}

// queryFilter holds the structural operators parsed out of a search query
// (kind:, role:, tool:). The remaining free text is what BM25 ranks against.
type queryFilter struct {
	Kinds    map[string]bool // allowed Block.Kind values
	Roles    map[string]bool // "user" / "assistant"
	Tools    map[string]bool // tool_use.Name values to require
	FreeText string          // query with operators stripped
}

// hasFilter reports whether any structural filter is active. When false, all
// turns pass turnMatchesQuery and we save a per-hit lowercase scan.
func (f queryFilter) hasFilter() bool {
	return len(f.Kinds) > 0 || len(f.Roles) > 0 || len(f.Tools) > 0
}

var operatorRE = regexp.MustCompile(`(?i)\b(kind|role|tool):([^\s]+)`)

// parseQueryOperators pulls `kind:X`, `role:X`, `tool:X` operators out of the
// raw query string. Operators may repeat (`tool:Bash tool:Grep` → both names
// allowed). Unknown operator values are kept as-is — the agent gets to see the
// empty result set rather than a silent fix-up.
func parseQueryOperators(q string) queryFilter {
	f := queryFilter{
		Kinds: map[string]bool{},
		Roles: map[string]bool{},
		Tools: map[string]bool{},
	}
	rest := operatorRE.ReplaceAllStringFunc(q, func(m string) string {
		colon := strings.IndexByte(m, ':')
		key := strings.ToLower(m[:colon])
		val := m[colon+1:]
		switch key {
		case "kind":
			f.Kinds[val] = true
		case "role":
			f.Roles[strings.ToLower(val)] = true
		case "tool":
			f.Tools[val] = true
		}
		return ""
	})
	f.FreeText = strings.TrimSpace(spaceRE.ReplaceAllString(rest, " "))
	return f
}

// turnMatchesQuery applies the structural filter to a Turn that BM25 already
// scored. We don't re-rank — BM25 ranks against the full search blob and the
// filter is a yes/no gate over the relevant scope. Within scope, all free-text
// terms must appear (case-insensitive substring), so `role:user "useEffect"`
// rejects turns where useEffect only appears in the assistant's reply.
func turnMatchesQuery(t Turn, f queryFilter) bool {
	if len(f.Tools) > 0 {
		hit := false
		for _, b := range t.Blocks {
			if b.Kind == BlockToolUse && f.Tools[b.Name] {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if len(f.Roles) == 0 && len(f.Kinds) == 0 {
		return true
	}

	// Determine which roles to consider.
	rolesSet := f.Roles
	if len(rolesSet) == 0 {
		rolesSet = map[string]bool{"user": true, "assistant": true}
	}

	var scope strings.Builder
	// User-side content is conceptually a single "text" block. Include it when
	// either no kind filter is set, or the filter explicitly allows BlockText.
	if rolesSet["user"] && (len(f.Kinds) == 0 || f.Kinds[BlockText]) {
		scope.WriteString(t.UserText)
		scope.WriteByte('\n')
	}
	if rolesSet["assistant"] {
		for _, b := range t.Blocks {
			if len(f.Kinds) > 0 && !f.Kinds[b.Kind] {
				continue
			}
			scope.WriteString(b.Render())
			scope.WriteByte('\n')
		}
	}

	// All free-text terms must appear in the scope.
	if f.FreeText == "" {
		return scope.Len() > 0
	}
	s := strings.ToLower(scope.String())
	for _, term := range strings.Fields(strings.ToLower(f.FreeText)) {
		if !strings.Contains(s, term) {
			return false
		}
	}
	return true
}
