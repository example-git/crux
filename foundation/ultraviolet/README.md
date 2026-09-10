# Ultraviolet

Adapted from `github.com/charmbracelet/ultraviolet`
`v0.0.0-20260703014108-f5a850f9c2b7` under its retained MIT license.

This package provides Crux's cell buffers, layout primitives, terminal input,
and incremental terminal renderer. It belongs to the root Crux module:

```go
import uv "github.com/example-git/crux/foundation/ultraviolet"
```

Both the application and `foundation/bubbletea` import it directly. There is
no nested module, replacement directive, or separate installation step.
Lip Gloss still has its own ordinary upstream Ultraviolet dependency; it does
not select the terminal renderer used by Crux's Bubble Tea program.

See [CRUX_PATCHES.md](CRUX_PATCHES.md) for the width guard and its regression
coverage, and [TUTORIAL.md](TUTORIAL.md) for the underlying APIs.
Run `CGO_ENABLED=0 go test ./foundation/ultraviolet/...` from the repository root.
