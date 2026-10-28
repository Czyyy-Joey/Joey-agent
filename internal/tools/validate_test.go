package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

const testSchema = `{
  "type": "object",
  "properties": {
    "path":  {"type": "string"},
    "limit": {"type": "integer", "minimum": 1},
    "mode":  {"type": "string", "enum": ["a", "b"]},
    "tags":  {"type": "array", "items": {"type": "string"}},
    "opts":  {"type": "object", "properties": {"deep": {"type": "boolean"}}, "additionalProperties": false}
  },
  "required": ["path"],
  "additionalProperties": false
}`

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		args string
		want []string // 为空表示期望通过
	}{
		{`{"path":"a"}`, nil},
		{`{"path":"a","limit":3,"mode":"b","tags":["x"],"opts":{"deep":true}}`, nil},
		{`{}`, []string{`missing required field "path"`, "expected parameters: path (string, required)"}},
		{``, []string{`missing required field "path"`}},
		{`{"path":"a","pth":1}`, []string{`unknown field "pth"`}},
		{`{"path":3}`, []string{"path must be a string, got a number"}},
		{`{"path":"a","limit":1.5}`, []string{"limit must be an integer"}},
		{`{"path":"a","limit":0}`, []string{"limit must be at least 1"}},
		{`{"path":"a","mode":"c"}`, []string{`mode must be one of "a", "b"`}},
		{`{"path":"a","tags":["x",2]}`, []string{"tags[1] must be a string"}},
		{`{"path":"a","opts":{"deep":"yes","x":1}}`, []string{"opts.deep must be a boolean", `unknown field "opts.x"`}},
		{`[]`, []string{"arguments must be an object, got an array"}},
	} {
		err := Validate(json.RawMessage(testSchema), json.RawMessage(tt.args))
		if len(tt.want) == 0 {
			if err != nil {
				t.Errorf("Validate(%s) = %v", tt.args, err)
			}
			continue
		}
		if err == nil || !IsModelError(err) {
			t.Errorf("Validate(%s) = %v, want a model error", tt.args, err)
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("Validate(%s) = %q, missing %q", tt.args, err, w)
			}
		}
	}
	if err := Validate(nil, json.RawMessage(`{"anything":1}`)); err != nil {
		t.Errorf("no schema: %v", err)
	}
}
