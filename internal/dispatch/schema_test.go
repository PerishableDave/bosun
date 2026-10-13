package dispatch

import (
	"encoding/json"
	"os"
	"testing"
)

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

func TestValidateArgs(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"to":{"type":"string","minLength":3},
			"n":{"type":"integer","minimum":1,"maximum":10},
			"f":{"type":"number"},
			"tags":{"type":"array","items":{"type":"string","enum":["a","b"]},"maxItems":2},
			"mode":{"enum":["fast","slow"]},
			"opt":{"type":["string","null"]},
			"meta":{"type":"object","additionalProperties":{"type":"string"}}
		},
		"required":["to"],
		"additionalProperties":false
	}`)
	ok := []string{
		`{"to":"abc"}`, `{"to":"abc","n":3,"f":1.5,"tags":["a"],"mode":"fast","opt":null,"meta":{"k":"v"}}`,
		`{"to":"abc","f":2}`, `{"to":"abc","opt":"x"}`,
	}
	bad := []string{
		`{}`, `{"to":"ab"}`, `{"to":5}`, `{"to":"abc","n":1.5}`, `{"to":"abc","n":0}`, `{"to":"abc","n":11}`,
		`{"to":"abc","tags":["c"]}`, `{"to":"abc","tags":["a","b","a"]}`, `{"to":"abc","mode":"medium"}`,
		`{"to":"abc","x":1}`, `{"to":"abc","opt":3}`, `{"to":"abc","meta":{"k":1}}`, `[]`, `"s"`, `{bad`,
	}
	for _, a := range ok {
		if err := ValidateArgs(schema, json.RawMessage(a)); err != nil {
			t.Errorf("%s: unexpected %v", a, err)
		}
	}
	for _, a := range bad {
		if err := ValidateArgs(schema, json.RawMessage(a)); err == nil {
			t.Errorf("%s: accepted", a)
		}
	}
	if err := ValidateArgs(nil, json.RawMessage(`{"anything":1}`)); err != nil {
		t.Errorf("empty schema: %v", err)
	}
	if err := ValidateArgs(json.RawMessage(`true`), json.RawMessage(`1`)); err != nil {
		t.Errorf("boolean schema: %v", err)
	}
}
