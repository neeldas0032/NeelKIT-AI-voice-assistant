package httpapi

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The spec and the router must agree in both directions.
//
// An endpoint that exists but is undocumented is invisible to anyone integrating
// against this; a documented endpoint that does not exist is worse, because it
// is trusted. Either one fails this test, which is the only way a hand-written
// spec stays true for longer than a fortnight.
//
// The YAML is scanned rather than parsed by a library on purpose: the project
// has three dependencies and adding a fourth to read a file with a known,
// fixed shape is a poor trade. The scanner understands exactly the two levels
// it needs — a path key at two spaces, a method key at four — and fails loudly
// on anything else.
func TestOpenAPIMatchesTheRouter(t *testing.T) {
	spec, err := parseOpenAPIOperations(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}

	h := newHarness(t)
	router := map[string]bool{}
	for _, pattern := range h.srv.Routes() {
		router[normalisePattern(pattern)] = true
	}

	if len(router) == 0 {
		t.Fatal("no routes registered; the test is not measuring anything")
	}

	for op := range router {
		if !spec[op] {
			t.Errorf("route %s is registered but missing from docs/openapi.yaml", op)
		}
	}
	for op := range spec {
		if !router[op] {
			t.Errorf("docs/openapi.yaml documents %s, which no route serves", op)
		}
	}

	if t.Failed() {
		t.Logf("registered routes:\n  %s", strings.Join(sortedKeys(router), "\n  "))
		t.Logf("documented operations:\n  %s", strings.Join(sortedKeys(spec), "\n  "))
	}
}

// normalisePattern turns a ServeMux pattern into "METHOD /path".
//
// Patterns registered without a method are not expected here; if one appears,
// returning it unchanged makes the mismatch obvious in the failure output rather
// than silently passing.
func normalisePattern(p string) string {
	parts := strings.Fields(p)
	if len(parts) != 2 {
		return p
	}
	return strings.ToUpper(parts[0]) + " " + parts[1]
}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true,
	"delete": true, "head": true, "options": true,
}

func parseOpenAPIOperations(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	ops := map[string]bool{}
	inPaths := false
	current := ""

	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Text()
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}

		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		trimmed := strings.TrimSpace(raw)

		// A top-level key ends the paths block.
		if indent == 0 {
			inPaths = trimmed == "paths:"
			current = ""
			continue
		}
		if !inPaths {
			continue
		}

		switch indent {
		case 2:
			if !strings.HasPrefix(trimmed, "/") || !strings.HasSuffix(trimmed, ":") {
				return nil, fmt.Errorf("line %d: expected a path key, got %q", line, trimmed)
			}
			current = strings.TrimSuffix(trimmed, ":")
		case 4:
			key := strings.TrimSuffix(trimmed, ":")
			if !httpMethods[key] {
				// Not a method — `parameters:`, `summary:` and friends live here
				// too. Skip rather than fail: the spec is allowed to grow.
				continue
			}
			if current == "" {
				return nil, fmt.Errorf("line %d: method %q outside any path", line, key)
			}
			ops[strings.ToUpper(key)+" "+current] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("no operations found; the scanner or the file shape changed")
	}
	return ops, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
