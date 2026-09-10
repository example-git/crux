# Bubble Tea

Adapted from `charm.land/bubbletea/v2` v2.0.8 under its retained MIT license.
This package belongs to Crux's root module and imports
`github.com/example-git/crux/foundation/ultraviolet` for terminal input and
rendering. In particular, `cursedRenderer.reset` constructs the native
Ultraviolet terminal renderer, including Crux's width guard.

Crux and its widgets in `foundation/bubbles` import this package directly so
the event loop, commands, input messages, and views use the same types.
No module replacement or alternate renderer selection is required.

Run `CGO_ENABLED=0 go test ./foundation/bubbletea` from the repository root.
