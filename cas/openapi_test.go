package cas

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIMatchesHandler sends one request per operation of
// api/openapi.yaml and checks that the answer is a documented response, so
// the descriptor and the routes cannot drift apart.
func TestOpenAPIMatchesHandler(t *testing.T) {
	raw, err := os.ReadFile("../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	s, _ := Open(t.TempDir())
	h := withTestKey(Handler(testAuth(s), slog.New(slog.DiscardHandler), true))
	_, body := call(t, h, "POST", "/objects", "spec object")
	hash := body[strings.Index(body, `"hash":"`)+8:][:64]

	// A valid request for each operation, by "METHOD path".
	requests := map[string]struct{ target, body string }{
		"POST /objects":                       {"/objects", "content"},
		"GET /objects":                        {"/objects?label=kind=pdf", ""},
		"GET /objects/{hash}":                 {"/objects/" + hash, ""},
		"HEAD /objects/{hash}":                {"/objects/" + hash, ""},
		"GET /objects/{hash}/labels":          {"/objects/" + hash + "/labels", ""},
		"PUT /objects/{hash}/labels/{key}":    {"/objects/" + hash + "/labels/kind", "pdf"},
		"DELETE /objects/{hash}/labels/{key}": {"/objects/" + hash + "/labels/kind", ""},
		"DELETE /objects/{hash}":              {"/objects/" + hash + "?unused_before=2000-01-01T00:00:00Z", ""},
		"GET /healthz":                        {"/healthz", ""},
	}
	seen := 0
	for path, ops := range spec.Paths {
		for method, op := range ops {
			if method == "parameters" {
				continue
			}
			name := strings.ToUpper(method) + " " + path
			req, ok := requests[name]
			if !ok {
				t.Errorf("%s: documented but not exercised by this test", name)
				continue
			}
			seen++
			var o struct {
				Responses map[string]yaml.Node `yaml:"responses"`
			}
			if err := op.Decode(&o); err != nil {
				t.Fatal(err)
			}
			code, _ := call(t, h, strings.ToUpper(method), req.target, req.body)
			if _, ok := o.Responses[strconv.Itoa(code)]; !ok {
				t.Errorf("%s answered %d, not documented", name, code)
			}
			if code >= http.StatusBadRequest {
				t.Errorf("%s: valid request answered %d", name, code)
			}
		}
	}
	if seen != len(requests) {
		t.Errorf("%d operations documented, %d expected", seen, len(requests))
	}
}
