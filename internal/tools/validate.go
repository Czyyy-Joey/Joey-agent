package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// schema 是 Validate 支持的 JSON Schema 子集：工具参数用到的只有这些。
type schema struct {
	Type                 string             `json:"type"`
	Properties           map[string]*schema `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties *bool              `json:"additionalProperties"`
	Enum                 []any              `json:"enum"`
	Items                *schema            `json:"items"`
	Minimum              *float64           `json:"minimum"`
}

// Validate 按工具的 JSON Schema 检查参数：必填字段、多余字段、类型、枚举、
// 最小值。失败时返回 *ModelError，消息里附上参数说明，方便模型改正
// （模型传错参数很常见，可操作的错误信息比一句 "invalid" 有用得多）。
//
// schema 为空或无法解析时不做检查，由工具自己的 DecodeArgs 兜底。
func Validate(rawSchema, args json.RawMessage) error {
	if len(rawSchema) == 0 {
		return nil
	}
	var s schema
	if err := json.Unmarshal(rawSchema, &s); err != nil {
		return nil
	}
	var v any
	if len(strings.TrimSpace(string(args))) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return Errorf("invalid arguments: %v", err)
	}
	var problems []string
	check(&s, v, "", &problems)
	if len(problems) == 0 {
		return nil
	}
	return Errorf("invalid arguments: %s\nexpected parameters: %s", strings.Join(problems, "; "), describe(&s))
}

func check(s *schema, v any, path string, problems *[]string) {
	name := path
	if name == "" {
		name = "arguments"
	}
	if s.Type != "" && !hasType(s.Type, v) {
		*problems = append(*problems, fmt.Sprintf("%s must be %s, got %s", name, article(s.Type), typeOf(v)))
		return
	}
	if len(s.Enum) > 0 && !slices.ContainsFunc(s.Enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(v) }) {
		*problems = append(*problems, fmt.Sprintf("%s must be one of %s, got %v", name, enumList(s.Enum), v))
	}
	if n, ok := v.(float64); ok && s.Minimum != nil && n < *s.Minimum {
		*problems = append(*problems, fmt.Sprintf("%s must be at least %v, got %v", name, *s.Minimum, n))
	}
	switch val := v.(type) {
	case map[string]any:
		for _, r := range s.Required {
			if _, ok := val[r]; !ok {
				*problems = append(*problems, fmt.Sprintf("missing required field %q", join(path, r)))
			}
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ps, ok := s.Properties[k]
			switch {
			case ok:
				check(ps, val[k], join(path, k), problems)
			case s.AdditionalProperties != nil && !*s.AdditionalProperties:
				*problems = append(*problems, fmt.Sprintf("unknown field %q", join(path, k)))
			}
		}
	case []any:
		if s.Items != nil {
			for i, e := range val {
				check(s.Items, e, fmt.Sprintf("%s[%d]", name, i), problems)
			}
		}
	}
}

func hasType(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == math.Trunc(n)
	}
	return true
}

func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	}
	return fmt.Sprintf("%T", v)
}

func article(t string) string {
	if t == "object" || t == "array" || t == "integer" {
		return "an " + t
	}
	return "a " + t
}

func enumList(e []any) string {
	parts := make([]string, len(e))
	for i, v := range e {
		parts[i] = fmt.Sprintf("%q", fmt.Sprint(v))
	}
	return strings.Join(parts, ", ")
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// describe 把顶层参数写成一行，例如 `path (string, required), offset (integer)`。
func describe(s *schema) string {
	if len(s.Properties) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := slices.Contains(s.Required, keys[i]), slices.Contains(s.Required, keys[j])
		if ri != rj {
			return ri
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		p := s.Properties[k]
		d := p.Type
		if len(p.Enum) > 0 {
			d = "one of " + enumList(p.Enum)
		}
		if slices.Contains(s.Required, k) {
			d += ", required"
		}
		parts[i] = fmt.Sprintf("%s (%s)", k, d)
	}
	return strings.Join(parts, ", ")
}
