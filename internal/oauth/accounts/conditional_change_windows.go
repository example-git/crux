//go:build windows

package accounts

// Windows does not expose Unix directory fsync semantics. The temporary file
// is synced before rename; directory durability follows the platform rename
// behavior, matching the repository's provider-plugin persistence contract.
func syncAccountDirectory(string) error { return nil }
