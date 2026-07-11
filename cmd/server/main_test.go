package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPasswordFromFile(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	password, err := loadPassword("", passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	if password != "secret-value" {
		t.Fatalf("got password %q", password)
	}
}

func TestLoadPasswordRejectsAmbiguousSources(t *testing.T) {
	if _, err := loadPassword("secret", "password.txt"); err == nil {
		t.Fatal("expected an error when both password sources are set")
	}
}

func TestLoadPasswordRejectsEmbeddedNewline(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPassword("", passwordFile); err == nil {
		t.Fatal("expected an embedded newline to be rejected")
	}
}

func TestParseServerConfigUsesEnvironment(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANYTLS_LISTEN", "127.0.0.1:9443")
	t.Setenv("ANYTLS_PASSWORD_FILE", passwordFile)
	t.Setenv("ANYTLS_FALLBACK", "")

	config, err := parseServerConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.listen != "127.0.0.1:9443" || config.password != "secret" || config.fallbackAddr != "" {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestParseServerConfigFlagsOverrideEnvironment(t *testing.T) {
	t.Setenv("ANYTLS_LISTEN", "127.0.0.1:9443")
	t.Setenv("ANYTLS_PASSWORD_FILE", "")

	config, err := parseServerConfig([]string{"-l", ":8443", "-p", "cli-secret", "-fallback", ""})
	if err != nil {
		t.Fatal(err)
	}
	if config.listen != ":8443" || config.password != "cli-secret" || config.fallbackAddr != "" {
		t.Fatalf("unexpected config: %+v", config)
	}
}
