// Package shellpreview resolves a named task to its command line.
//
// It exists as its own package so that the runner's approval preview and the
// shell tool's execution read the task table from the same code, without the
// runner importing the tool (which would drag every tool into every binary
// that only wants to render a preview).
package shellpreview

import "strings"

// DefaultTasks mirrors the shell tool's defaults.
var DefaultTasks = map[string]string{
	"test":  "go test ./...",
	"build": "go build ./...",
	"lint":  "go vet ./...",
	"fmt":   "gofmt -l .",
}

// Resolve returns the command line for a task name.
func Resolve(cfg map[string]string, name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if v, ok := cfg["task."+name]; ok && v != "" {
		return v, true
	}
	v, ok := DefaultTasks[name]
	return v, ok
}
