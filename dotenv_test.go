package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		in, key, val string
		ok           bool
	}{
		{"AUDIO_MAX_SECS=10", "AUDIO_MAX_SECS", "10", true},
		{"  export PREFIX = .sa  ", "PREFIX", ".sa", true},
		{`PAIR_PHONE="628123"`, "PAIR_PHONE", "628123", true},
		{`PREFIX='.sa # x'`, "PREFIX", ".sa # x", true},
		{"LOG_LEVEL=debug # noisy", "LOG_LEVEL", "debug", true},
		{"EMPTY=", "EMPTY", "", true},
		{"# comment", "", "", false},
		{"", "", "", false},
		{"no equals sign", "", "", false},
		{"=novalue", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseEnvLine(c.in)
		if k != c.key || v != c.val || ok != c.ok {
			t.Errorf("parseEnvLine(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, k, v, ok, c.key, c.val, c.ok)
		}
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "SA_TEST_A=from-file\nSA_TEST_B=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("SA_TEST_A"); os.Unsetenv("SA_TEST_B") })
	t.Setenv("SA_TEST_B", "from-shell") // the real environment must win

	n, err := loadDotEnv(path)
	if err != nil || n != 1 {
		t.Fatalf("loadDotEnv = (%d, %v), want (1, nil)", n, err)
	}
	if got := os.Getenv("SA_TEST_A"); got != "from-file" {
		t.Errorf("SA_TEST_A = %q", got)
	}
	if got := os.Getenv("SA_TEST_B"); got != "from-shell" {
		t.Errorf("SA_TEST_B = %q, shell value should win", got)
	}

	if n, err := loadDotEnv(filepath.Join(t.TempDir(), "missing.env")); n != 0 || err != nil {
		t.Errorf("missing file = (%d, %v), want (0, nil)", n, err)
	}
}
