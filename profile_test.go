package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCodexRuntimeCustomProvider(t *testing.T) {
	t.Parallel()
	content := `model = "gemini-test"

[chat_model]
provider = "openai-compatible"
base_url = "https://provider.example/v1"

[provider.openai-compatible]
api_key = "secret-token"
wire_api = "responses"
`
	runtime, err := parseCodexRuntime(content)
	if err != nil {
		t.Fatalf("parseCodexRuntime: %v", err)
	}
	if !runtime.Custom || runtime.APIKey != "secret-token" || runtime.BaseURL != "https://provider.example/v1" || runtime.Model != "gemini-test" {
		t.Fatalf("unexpected runtime: %#v", runtime)
	}
	for _, secretField := range []string{"secret-token", "api_key", "base_url", "wire_api"} {
		if strings.Contains(runtime.ProfileContent, secretField) {
			t.Fatalf("generated profile contains %q: %s", secretField, runtime.ProfileContent)
		}
	}
	args := strings.Join(customProviderArgs(runtime), " ")
	if strings.Contains(args, runtime.APIKey) {
		t.Fatal("API key leaked into command arguments")
	}
	for _, expected := range []string{"model_provider", "provider.example", "OPENAI_API_KEY", "shell_snapshot"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("missing custom provider argument %q", expected)
		}
	}
}

func TestParseCodexRuntimePreservesNativeNestedProvider(t *testing.T) {
	t.Parallel()
	content := `model = "native-model"

[model_providers.native]
name = "Native provider"
base_url = "https://provider.example/v1"
env_key = "NATIVE_API_KEY"
wire_api = "responses"
`
	runtime, err := parseCodexRuntime(content)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Custom {
		t.Fatal("nested native provider was misclassified as EasyConnect shorthand")
	}
	for _, expected := range []string{"model_providers.native", "base_url", "env_key", "wire_api"} {
		if !strings.Contains(runtime.ProfileContent, expected) {
			t.Fatalf("native profile lost %q: %s", expected, runtime.ProfileContent)
		}
	}
}

func TestParseCodexRuntimePreservesUnrelatedNestedAPIKey(t *testing.T) {
	t.Parallel()
	content := `model = "native-model"

[mcp_servers.example]
command = "example-mcp"
api_key = "tool-specific-value"
`
	runtime, err := parseCodexRuntime(content)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Custom {
		t.Fatal("unrelated nested api_key was misclassified as provider shorthand")
	}
	if !strings.Contains(runtime.ProfileContent, `api_key = "tool-specific-value"`) {
		t.Fatalf("unrelated nested field was removed: %s", runtime.ProfileContent)
	}
}

func TestParseCodexRuntimeValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "blank template is ordinary profile", content: "api_key = \"\"\nbase_url = \"\"\nmodel = \"\"", wantErr: ""},
		{name: "partial custom provider", content: "api_key = \"token\"\nmodel = \"model\"", wantErr: "require api_key"},
		{name: "invalid URL", content: "api_key = \"token\"\nbase_url = \"file:///tmp/api\"\nmodel = \"model\"", wantErr: "http or https"},
		{name: "invalid wire API", content: "api_key = \"token\"\nbase_url = \"https://example.test/v1\"\nmodel = \"model\"\nwire_api = \"legacy\"", wantErr: "responses or chat"},
		{name: "invalid TOML", content: `model = [`, wantErr: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseCodexRuntime(test.content)
			if test.name == "invalid TOML" {
				if err == nil {
					t.Fatal("expected TOML error")
				}
				return
			}
			if test.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestSafeCodexErrorRedactsCredential(t *testing.T) {
	t.Parallel()
	secret := "do-not-return-this-token"
	message := safeCodexError([]byte("warning\nError: provider rejected "+secret+"\n"), secret)
	if strings.Contains(message, secret) || !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("unsafe validation error: %q", message)
	}
}

func TestEnsureDirectoryOwnsAllNewPathComponents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "one", "two", "three")
	if err := ensureDirectory(path, 0o700, ""); err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{filepath.Join(root, "one"), filepath.Join(root, "one", "two"), path} {
		info, err := os.Stat(component)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory %s: info=%v err=%v", component, info, err)
		}
	}
}
