package llm

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// AnthropicVendor is the default vendor, selected whenever no --vendor flag
// is given.
const AnthropicVendor = "anthropic"

// ClaudeCodeVendor runs the local `claude` command instead of calling an API,
// for a machine whose only model access is a Claude Code subscription. It is
// the one vendor with no secret of its own: the command holds its own login.
const ClaudeCodeVendor = "claude-code"

// NormalizeVendor turns a raw --vendor flag value into a lowercase vendor
// name, defaulting to AnthropicVendor when the flag was not given. isDefault
// tells a caller whether the default was applied, so a report can say so
// rather than staying silent about it.
func NormalizeVendor(flagValue string) (vendor string, isDefault bool) {
	trimmed := strings.ToLower(strings.TrimSpace(flagValue))
	if trimmed == "" {
		return AnthropicVendor, true
	}
	return trimmed, false
}

// Describe reports the vendor a --vendor flag value selects, in the form
// Inspector records in its stamps and reports so the choice is never silent:
// "anthropic (default, no flag given)" or the vendor name itself.
func Describe(flagValue string) string {
	vendor, isDefault := NormalizeVendor(flagValue)
	if isDefault {
		return vendor + " (default, no flag given)"
	}
	return vendor
}

// EnvVarNames returns the one fixed rule mapping a vendor to its secret
// names: an API key for every vendor, and a base URL for every vendor except
// Anthropic (whose address is fixed). Names only - no value is read here.
//
// ClaudeCodeVendor is the one exception, and returns neither name: it reaches
// the model by running a command that already holds its own login, so there
// is no secret for Inspector to read and none to forward. A caller that
// forwards these names to somewhere else must therefore check for an empty
// name rather than assume there is always one.
func EnvVarNames(flagValue string) (vendor, apiKeyVar, baseURLVar string) {
	vendor, _ = NormalizeVendor(flagValue)
	if vendor == ClaudeCodeVendor {
		return vendor, "", ""
	}
	upper := strings.ToUpper(vendor)
	apiKeyVar = upper + "_API_KEY"
	if vendor != AnthropicVendor {
		baseURLVar = upper + "_BASE_URL"
	}
	return vendor, apiKeyVar, baseURLVar
}

// Resolve builds the adapter for a --vendor flag value, reading its secrets
// by name from the environment - never accepted on a command line. Anthropic
// reads only its API key and talks to the fixed Anthropic API address.
// claude-code reads no secret at all and runs the local `claude` command,
// which is looked up on PATH here so a missing one is refused by name before
// any judgment starts rather than part-way through it.
// Every other vendor is treated as an OpenAI-compatible endpoint: it reads an
// API key and a base URL, both required, so it can reach xAI, OpenRouter, a
// local Ollama, or any other address that speaks that request shape.
func Resolve(flagValue, model string) (Adapter, string, error) {
	vendor, apiKeyVar, baseURLVar := EnvVarNames(flagValue)
	if vendor == ClaudeCodeVendor {
		if _, err := exec.LookPath(ClaudeCodeCommand); err != nil {
			return nil, vendor, fmt.Errorf("the %s command is not on PATH, so vendor %s has no way to reach a model: %v", ClaudeCodeCommand, vendor, err)
		}
		return &ClaudeCodeAdapter{Model: model}, vendor, nil
	}
	apiKey := os.Getenv(apiKeyVar)
	if strings.TrimSpace(apiKey) == "" {
		return nil, vendor, fmt.Errorf("%s is not set", apiKeyVar)
	}
	if vendor == AnthropicVendor {
		return &AnthropicAdapter{APIKey: apiKey, Model: model}, vendor, nil
	}
	baseURL := os.Getenv(baseURLVar)
	if strings.TrimSpace(baseURL) == "" {
		return nil, vendor, fmt.Errorf("%s is not set", baseURLVar)
	}
	return &OpenAIAdapter{APIKey: apiKey, BaseURL: baseURL, Model: model}, vendor, nil
}
