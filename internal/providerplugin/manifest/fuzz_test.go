package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

func FuzzDecodeStrict(f *testing.F) {
	f.Add(readRepoFileForFuzz(f, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"))
	f.Add([]byte(`{"manifest_version":1}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"unknown":true} {}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxManifestBytes+1 {
			t.Skip()
		}
		value, err := DecodeStrict(data)
		if err == nil {
			if err := Validate(value); err != nil {
				t.Fatalf("DecodeStrict returned a semantically invalid manifest: %v", err)
			}
		}
	})
}

func FuzzSafeBundlePath(f *testing.F) {
	for _, seed := range []string{
		"instructions/native.txt",
		"",
		".",
		"..",
		"../escape",
		"a/../escape",
		"/absolute",
		`C:\\escape`,
		"a//b",
		"a/./b",
		"nul\x00byte",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		valid := safeBundlePath(value)
		if valid && (!utf8.ValidString(value) || value == "" || value[0] == '/') {
			t.Fatalf("unsafe path accepted: %q", value)
		}
	})
}

func readRepoFileForFuzz(tb testing.TB, parts ...string) []byte {
	tb.Helper()
	all := append([]string{"..", "..", ".."}, parts...)
	data, err := os.ReadFile(filepath.Join(all...))
	if err != nil {
		tb.Fatalf("read fuzz seed: %v", err)
	}
	return data
}
