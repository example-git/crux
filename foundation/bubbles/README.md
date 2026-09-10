# Bubbles

Adapted from `charm.land/bubbles/v2` v2.1.1 under its retained MIT license.
Only the components used by Crux and their supporting packages are included:
cursor, filepicker, help, key, spinner, textarea, textinput, viewport,
internal/memoization, and internal/runeutil.

These are ordinary packages in Crux's root module. They import
`foundation/bubbletea` directly so their commands and messages belong to the
same event loop as Crux. Lip Gloss remains a normal styling dependency.

Run `CGO_ENABLED=0 go test ./foundation/bubbles/...` from the repository root.
