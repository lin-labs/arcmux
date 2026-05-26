package mux

import "errors"

// ErrUnsupported is returned by backends for operations that don't translate
// to their native multiplexer (e.g. cmux browser panes when called through
// the interface, tmux pane splitting when given a non-empty Direction).
var ErrUnsupported = errors.New("mux: operation not supported by backend")
