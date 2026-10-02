package main

// The tool set this binary ships with.
//
// Each import runs the package's init, which registers its tools. This list is
// the plugin manifest: adding a capability means writing the tool in its own
// package and adding one line here. Nothing in the assistant, the planner, the
// API or the UI changes.
//
// It is the same pattern the standard library uses for database/sql drivers and
// image decoders, which means a Go reviewer recognises it without explanation.
import (
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/fsx"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/git"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/notes"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/search"
	_ "github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/tools/builtin/shell"
)
