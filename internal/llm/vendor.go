package llm

import (
	"fmt"
	"os"
	"strings"
)

// AnthropicVendor is the default vendor, selected whenever no --vendor flag
// is given.
const AnthropicVendor = "anthropic"

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
func EnvVarNames(flagValue string) (vendor, apiKeyVar, baseURLVar string) {
	vendor, _ = NormalizeVendor(flagValue)
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
// Every other vendor is treated as an OpenAI-compatible endpoint: it reads an
// API key and a base URL, both required, so it can reach xAI, OpenRouter, a
// local Ollama, or any other address that speaks that request shape.
func Resolve(flagValue, model string) (Adapter, string, error) {
	vendor, apiKeyVar, baseURLVar := EnvVarNames(flagValue)
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
