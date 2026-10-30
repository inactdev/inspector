package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeVendor_DefaultsToAnthropic(t *testing.T) {
	vendor, isDefault := NormalizeVendor("")
	if vendor != AnthropicVendor || !isDefault {
		t.Fatalf("NormalizeVendor(\"\") = (%q, %v), want (%q, true)", vendor, isDefault, AnthropicVendor)
	}
	vendor, isDefault = NormalizeVendor("  OpenRouter  ")
	if vendor != "openrouter" || isDefault {
		t.Fatalf("NormalizeVendor(\" OpenRouter \") = (%q, %v), want (openrouter, false)", vendor, isDefault)
	}
}

func TestDescribe_NamesTheDefaultExplicitly(t *testing.T) {
	if got := Describe(""); got != "anthropic (default, no flag given)" {
		t.Fatalf("Describe(\"\") = %q, want the explicit default marker", got)
	}
	if got := Describe("xai"); got != "xai" {
		t.Fatalf("Describe(\"xai\") = %q, want xai", got)
	}
}

func TestEnvVarNames_FollowsTheOneFixedRule(t *testing.T) {
	vendor, apiKeyVar, baseURLVar := EnvVarNames("")
	if vendor != "anthropic" || apiKeyVar != "ANTHROPIC_API_KEY" || baseURLVar != "" {
		t.Fatalf("EnvVarNames(\"\") = (%q, %q, %q), want anthropic with no base URL variable", vendor, apiKeyVar, baseURLVar)
	}
	vendor, apiKeyVar, baseURLVar = EnvVarNames("openrouter")
	if vendor != "openrouter" || apiKeyVar != "OPENROUTER_API_KEY" || baseURLVar != "OPENROUTER_BASE_URL" {
		t.Fatalf("EnvVarNames(\"openrouter\") = (%q, %q, %q), want OPENROUTER_API_KEY and OPENROUTER_BASE_URL", vendor, apiKeyVar, baseURLVar)
	}
}

func TestResolve_RefusesByExactMissingVariableName(t *testing.T) {
	if _, _, err := Resolve("openrouter", "some-model"); err == nil || err.Error() != "OPENROUTER_API_KEY is not set" {
		t.Fatalf("Resolve() error = %v, want a refusal naming OPENROUTER_API_KEY", err)
	}
	t.Setenv("OPENROUTER_API_KEY", "key")
	if _, _, err := Resolve("openrouter", "some-model"); err == nil || err.Error() != "OPENROUTER_BASE_URL is not set" {
		t.Fatalf("Resolve() error = %v, want a refusal naming OPENROUTER_BASE_URL", err)
	}
	t.Setenv("OPENROUTER_BASE_URL", "http://127.0.0.1:1")
	adapter, vendor, err := Resolve("openrouter", "some-model")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if vendor != "openrouter" {
		t.Fatalf("vendor = %q, want openrouter", vendor)
	}
	if _, ok := adapter.(*OpenAIAdapter); !ok {
		t.Fatalf("adapter = %T, want *OpenAIAdapter", adapter)
	}
}

func TestEnvVarNames_ClaudeCodeHasNoSecretOfItsOwn(t *testing.T) {
	vendor, apiKeyVar, baseURLVar := EnvVarNames("Claude-Code")
	if vendor != ClaudeCodeVendor || apiKeyVar != "" || baseURLVar != "" {
		t.Fatalf("EnvVarNames(\"Claude-Code\") = (%q, %q, %q), want %q with no variable names at all", vendor, apiKeyVar, baseURLVar, ClaudeCodeVendor)
	}
}

func TestResolve_ClaudeCodeNeedsTheCommandAndNoKey(t *testing.T) {
	// A PATH holding nothing: the refusal has to name the command, because
	// "no API key" would send someone looking for a key that does not exist.
	t.Setenv("PATH", t.TempDir())
	if _, _, err := Resolve("claude-code", "claude-sonnet-5"); err == nil || !strings.Contains(err.Error(), ClaudeCodeCommand) {
		t.Fatalf("Resolve() error = %v, want a refusal naming the %s command", err, ClaudeCodeCommand)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ClaudeCodeCommand), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing the stand-in command: %v", err)
	}
	t.Setenv("PATH", dir)
	// No <VENDOR>_API_KEY and no <VENDOR>_BASE_URL anywhere, deliberately.
	adapter, vendor, err := Resolve("claude-code", "claude-sonnet-5")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if vendor != ClaudeCodeVendor {
		t.Fatalf("vendor = %q, want %q", vendor, ClaudeCodeVendor)
	}
	claudeCode, ok := adapter.(*ClaudeCodeAdapter)
	if !ok {
		t.Fatalf("adapter = %T, want *ClaudeCodeAdapter", adapter)
	}
	// The clarification on issue #70: the vendor's model setting is passed
	// through rather than left to the subscription's own default.
	if claudeCode.Model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want the --model value passed through", claudeCode.Model)
	}
}

func TestResolve_AnthropicNeedsOnlyItsAPIKey(t *testing.T) {
	if _, _, err := Resolve("", "some-model"); err == nil || err.Error() != "ANTHROPIC_API_KEY is not set" {
		t.Fatalf("Resolve() error = %v, want a refusal naming ANTHROPIC_API_KEY", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "key")
	adapter, vendor, err := Resolve("", "some-model")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if vendor != AnthropicVendor {
		t.Fatalf("vendor = %q, want %q", vendor, AnthropicVendor)
	}
	if _, ok := adapter.(*AnthropicAdapter); !ok {
		t.Fatalf("adapter = %T, want *AnthropicAdapter", adapter)
	}
}
