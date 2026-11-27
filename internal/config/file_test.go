package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolvePath(t *testing.T) {
	t.Setenv(EnvConfigPath, "/env/config.yaml")
	if got := ResolvePath(" /flag/config.yaml "); got != "/flag/config.yaml" {
		t.Fatalf("ResolvePath(flag) = %q, want /flag/config.yaml", got)
	}
	if got := ResolvePath(" "); got != "/env/config.yaml" {
		t.Fatalf("ResolvePath(env) = %q, want /env/config.yaml", got)
	}
	t.Setenv(EnvConfigPath, "")
	if got := ResolvePath(""); got != "" {
		t.Fatalf("ResolvePath(empty) = %q, want empty", got)
	}
}

func TestLoadFileAndDecodeSection(t *testing.T) {
	t.Run("empty path returns empty file", func(t *testing.T) {
		f, err := LoadFile("")
		if err != nil {
			t.Fatalf("LoadFile empty: %v", err)
		}
		if f == nil || f.Database.Kind != 0 {
			t.Fatalf("expected empty File, got %+v", f)
		}
	})

	t.Run("comments only yaml returns empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "comments.yaml")
		if err := os.WriteFile(path, []byte("# only comment\n\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile comments: %v", err)
		}
		if f == nil || f.HTTP.Kind != 0 {
			t.Fatalf("expected empty File, got %+v", f)
		}
	})

	t.Run("missing file returns error", func(t *testing.T) {
		if _, err := LoadFile(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("rejects unknown top-level key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unknown-top.yaml")
		if err := os.WriteFile(path, []byte("unknown_section:\n  foo: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "unknown_section") {
			t.Fatalf("err = %v, want unknown_section", err)
		}
	})

	t.Run("rejects multi-document yaml", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "multi.yaml")
		if err := os.WriteFile(path, []byte("log:\n  level: info\n---\nlog:\n  level: debug\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "single YAML document") {
			t.Fatalf("err = %v, want single YAML document", err)
		}
	})

	t.Run("decode section rejects unknown nested key and keeps defaults on null", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested.yaml")
		content := "database: null\nhttp:\n  bogus: 1\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}

		type dbCfg struct {
			MaxOpen int `yaml:"max_open_conns"`
		}
		gotDB := dbCfg{MaxOpen: 10}
		if err := DecodeSection(f.Database, &gotDB); err != nil {
			t.Fatalf("DecodeSection(null) error = %v", err)
		}
		if gotDB.MaxOpen != 10 {
			t.Fatalf("null section overwrote default: got %d, want 10", gotDB.MaxOpen)
		}

		var gotHTTP httpSection
		if err := DecodeSection(f.HTTP, &gotHTTP); err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Fatalf("DecodeSection(unknown field) err = %v, want bogus", err)
		}
	})

	t.Run("duration rejects bare integer and accepts duration string", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dur.yaml")
		if err := os.WriteFile(path, []byte("http:\n  request_timeout: 5\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var h httpSection
		if err := DecodeSection(f.HTTP, &h); err == nil || !strings.Contains(err.Error(), "duration must be a string") {
			t.Fatalf("bare int duration err = %v", err)
		}

		if err := os.WriteFile(path, []byte("http:\n  request_timeout: 12s\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err = LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		h = httpSection{}
		if err := DecodeSection(f.HTTP, &h); err != nil {
			t.Fatalf("valid duration string err = %v", err)
		}
		if h.RequestTimeout == nil || h.RequestTimeout.Std() != 12*time.Second {
			t.Fatalf("got = %v, want 12s", h.RequestTimeout)
		}
	})

	t.Run("lookup mapping string", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lookup.yaml")
		if err := os.WriteFile(path, []byte("console:\n  public_url: https://console.example.com\n  ip_rate_limit: 50\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := LookupMappingString(f.Console, "public_url")
		if err != nil || !ok || got != "https://console.example.com" {
			t.Fatalf("LookupMappingString(public_url) = (%q, %v, %v)", got, ok, err)
		}
		if _, ok, err := LookupMappingString(f.Console, "missing"); err != nil || ok {
			t.Fatalf("LookupMappingString(missing) = (%v, %v)", ok, err)
		}
		if _, _, err := LookupMappingString(f.Console, "ip_rate_limit"); err == nil {
			t.Fatal("expected error for non-string field")
		}
	})
}
