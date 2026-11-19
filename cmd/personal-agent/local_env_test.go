package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseLocalEnvQuotedValuesCommentsAndLiteralShellSyntax(t *testing.T) {
	input := "\ufeff# local development only\r\n" +
		"export EXAMPLE_LABEL = \"label-with-spaces v1\" # comment\n" +
		"EXAMPLE_URL=https://example.test/v1#fragment\n" +
		"EXAMPLE_TOKEN='synthetic # value $HOME' # comment\n" +
		"EMPTY= # empty value\n" +
		"LITERAL=$(printf synthetic) ${HOME} `printf synthetic`\n" +
		"ESCAPED=\"line\\nnext\\t\\\"quoted\\\"\\\\end\"\n" +
		"UNKNOWN_ESCAPE=\"keep\\q\"\n" +
		"UNICODE='华东渠道'\n" +
		"DUPLICATE=first\nDUPLICATE=last\n"
	got, err := parseLocalEnv(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"EXAMPLE_LABEL":  "label-with-spaces v1",
		"EXAMPLE_URL":    "https://example.test/v1#fragment",
		"EXAMPLE_TOKEN":  "synthetic # value $HOME",
		"EMPTY":          "",
		"LITERAL":        "$(printf synthetic) ${HOME} `printf synthetic`",
		"ESCAPED":        "line\nnext\t\"quoted\"\\end",
		"UNKNOWN_ESCAPE": "keep\\q",
		"UNICODE":        "华东渠道",
		"DUPLICATE":      "last",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed local environment does not match expected values: %#v", got)
	}
}

func TestParseLocalEnvErrorsDoNotExposeConfigurationValues(t *testing.T) {
	for _, input := range []string{
		"TOKEN=\"SENSITIVE-CONTENT",
		"TOKEN='SENSITIVE-CONTENT' invalid",
		"SENSITIVE-CONTENT",
		"INVALID-KEY=SENSITIVE-CONTENT",
		"TOKEN=SENSITIVE-CONTENT\x00",
	} {
		_, err := parseLocalEnv(strings.NewReader(input))
		if err == nil || strings.Contains(err.Error(), "SENSITIVE-CONTENT") || !strings.Contains(err.Error(), "line 1") {
			t.Fatalf("configuration error must identify the line without including its content: %v", err)
		}
	}
}

func TestLoadLocalEnvPreservesExportedVariablesIncludingEmpty(t *testing.T) {
	const fresh = "SOP_AGENT_TEST_FROM_LOCAL_FILE"
	t.Setenv(fresh, "cleanup-placeholder")
	if err := os.Unsetenv(fresh); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOP_AGENT_TEST_EXPORTED", "already-exported")
	t.Setenv("SOP_AGENT_TEST_EXPORTED_EMPTY", "")
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	path := filepath.Join(t.TempDir(), "local.env")
	content := fresh + "=$(touch " + marker + ")\n" +
		"SOP_AGENT_TEST_EXPORTED=from-file\n" +
		"SOP_AGENT_TEST_EXPORTED_EMPTY=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadLocalEnv(path, true); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(fresh); got != "$(touch "+marker+")" {
		t.Fatalf("shell syntax must be preserved as literal data, got %q", got)
	}
	if os.Getenv("SOP_AGENT_TEST_EXPORTED") != "already-exported" || os.Getenv("SOP_AGENT_TEST_EXPORTED_EMPTY") != "" {
		t.Fatal("local.env overwrote an already exported variable")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("loading configuration must not execute shell commands")
	}
}

func TestLoadLocalEnvMissingFileAndMalformedFileAreHandledSafely(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.env")
	if err := loadLocalEnv(missing, false); err != nil {
		t.Fatalf("missing default file should be optional: %v", err)
	}
	if err := loadLocalEnv(missing, true); err == nil {
		t.Fatal("an explicitly requested missing file must fail")
	}
	const key = "SOP_AGENT_TEST_NO_PARTIAL_LOAD"
	t.Setenv(key, "cleanup-placeholder")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "local.env")
	if err := os.WriteFile(path, []byte(key+"=value\nINVALID-KEY=do-not-load\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadLocalEnv(path, true); err == nil {
		t.Fatal("malformed configuration must fail")
	}
	if _, exists := os.LookupEnv(key); exists {
		t.Fatal("malformed configuration must not be partially applied")
	}
}
