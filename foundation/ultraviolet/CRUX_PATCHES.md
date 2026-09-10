# Crux renderer patch

This directory retains the source, license, and tests of
`github.com/charmbracelet/ultraviolet` at
`v0.0.0-20260703014108-f5a850f9c2b7`.
This is an ordinary package in Crux's root module. Crux's screen composition
and `foundation/bubbletea` both import `foundation/ultraviolet` directly;
there is no nested module or `go.mod` replacement.

Local change: `terminal_renderer_width_guard.go` and its call from
`transformLine` prevent terminal character-width disagreement from corrupting
adjacent panes. Changed rows containing wide or combined characters are
painted with autowrap disabled and horizontal cursor positioning after each
such cell. The remaining row is repainted to repair any overdraw. Unchanged
rows and ordinary single-column text keep the existing incremental behavior.
The protection also applies to initial/full redraws and inline rendering.
Allocated wide cells are erased before writing to avoid stale continuation
columns when the terminal renders a cluster narrower than expected. Cursor
movement must not reprint wide/combined cells as an optimization.

The reproduction uses Crux's real `UI.View` frames and the demo's embedded
virtual terminal with terminal state retained between updates. See
`internal/ui/demo/sidebar_redraw_test.go` and `terminal_width_guard_test.go`
in the root module.

Keep this behavior when updating the adapted source. Run its tests from the
root module with `CGO_ENABLED=0 go test ./foundation/ultraviolet/...`.
