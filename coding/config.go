package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/openai"
)

// ProviderConfig describes one model endpoint. Built-in presets can be
// overridden or extended field-by-field from settings.json.
type ProviderConfig struct {
	Name            string            `json:"-"`
	DisplayName     string            `json:"displayName,omitempty"`
	API             string            `json:"api,omitempty"` // only "openai-completions" today
	BaseURL         string            `json:"baseUrl,omitempty"`
	APIKey          string            `json:"apiKey,omitempty"`
	APIKeyEnv       string            `json:"apiKeyEnv,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	SessionHeader   string            `json:"sessionHeader,omitempty"` // header carrying the session id
	Models          []string          `json:"models,omitempty"`
	DefaultModel    string            `json:"defaultModel,omitempty"`
	VisionModels    []string          `json:"visionModels,omitempty"`
	ThinkingFormat  string            `json:"thinkingFormat,omitempty"`
	ThinkingLevels  []string          `json:"thinkingLevels,omitempty"`
	DefaultThinking string            `json:"defaultThinking,omitempty"`
	MaxTokens       int               `json:"maxTokens,omitempty"`
}

// Settings is ~/.malachi/settings.json.
type Settings struct {
	DefaultProvider    string                    `json:"defaultProvider,omitempty"`
	DefaultModel       string                    `json:"defaultModel,omitempty"`
	ThinkingLevel      string                    `json:"thinkingLevel,omitempty"`
	AppendSystemPrompt string                    `json:"appendSystemPrompt,omitempty"`
	Icons              string                    `json:"icons,omitempty"` // TUI icon set: "emoji" (default) or "dots"
	Providers          map[string]ProviderConfig `json:"providers,omitempty"`
}

var standardThinking = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// BuiltinProviders are presets that work with only an API key.
func BuiltinProviders() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"opencode-go": {
			DisplayName: "OpenCode Go",
			API:         openai.API,
			BaseURL:     "https://opencode.ai/zen/go/v1",
			APIKeyEnv:   "OPENCODE_API_KEY",
			// Required by OpenCode Go for routing and prompt caching:
			// https://opencode.ai/docs/go/#where-can-i-use-it
			SessionHeader: "x-opencode-session",
			Models: []string{
				"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.1", "glm-5.2", "kimi-k2.6", "kimi-k2.7-code",
				"mimo-v2.5", "mimo-v2.5-pro", "minimax-m2.7", "minimax-m3", "qwen3.6-plus", "qwen3.7-max", "qwen3.7-plus",
			},
			DefaultModel:    "kimi-k2.7-code",
			VisionModels:    []string{"kimi-k2.6", "kimi-k2.7-code", "mimo-v2.5", "minimax-m3", "qwen3.6-plus", "qwen3.7-plus"},
			ThinkingFormat:  openai.ThinkingOpenAI,
			ThinkingLevels:  standardThinking,
			DefaultThinking: "medium",
		},
		"openai": {
			DisplayName:     "OpenAI",
			API:             openai.API,
			BaseURL:         "https://api.openai.com/v1",
			APIKeyEnv:       "OPENAI_API_KEY",
			DefaultModel:    "gpt-5.1",
			ThinkingFormat:  openai.ThinkingOpenAI,
			ThinkingLevels:  standardThinking,
			DefaultThinking: "medium",
		},
		"openrouter": {
			DisplayName:     "OpenRouter",
			API:             openai.API,
			BaseURL:         "https://openrouter.ai/api/v1",
			APIKeyEnv:       "OPENROUTER_API_KEY",
			ThinkingFormat:  openai.ThinkingOpenRouter,
			ThinkingLevels:  standardThinking,
			DefaultThinking: "medium",
		},
		"ollama": {
			DisplayName:    "Ollama",
			API:            openai.API,
			BaseURL:        "http://localhost:11434/v1",
			ThinkingFormat: openai.ThinkingOpenAI,
			ThinkingLevels: []string{"off"},
		},
	}
}

// Version is malachi's release version, sent in the User-Agent.
const Version = "0.1.0"

// UserAgent identifies malachi to providers, as gateways like OpenCode Go
// require a client-specific agent rather than a generic HTTP library name.
var UserAgent = "malachi/" + Version

// Home returns malachi's state directory: $MALACHI_HOME or ~/.malachi.
func Home() string {
	if h := os.Getenv("MALACHI_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".malachi")
	}
	return ".malachi"
}

// LoadSettings reads home/settings.json; a missing file yields defaults.
func LoadSettings(home string) (*Settings, error) {
	s := &Settings{}
	data, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	return s, nil
}

// ProviderConfigs merges settings over the built-in presets.
func (s *Settings) ProviderConfigs() map[string]ProviderConfig {
	out := BuiltinProviders()
	for name, o := range s.Providers {
		base := out[name]
		mergeProvider(&base, o)
		if base.API == "" {
			base.API = openai.API
		}
		out[name] = base
	}
	for name, pc := range out {
		pc.Name = name
		out[name] = pc
	}
	return out
}

func mergeProvider(dst *ProviderConfig, o ProviderConfig) {
	set := func(d *string, v string) {
		if v != "" {
			*d = v
		}
	}
	set(&dst.DisplayName, o.DisplayName)
	set(&dst.API, o.API)
	set(&dst.BaseURL, o.BaseURL)
	set(&dst.APIKey, o.APIKey)
	set(&dst.APIKeyEnv, o.APIKeyEnv)
	set(&dst.SessionHeader, o.SessionHeader)
	set(&dst.DefaultModel, o.DefaultModel)
	set(&dst.ThinkingFormat, o.ThinkingFormat)
	set(&dst.DefaultThinking, o.DefaultThinking)
	if o.Models != nil {
		dst.Models = o.Models
	}
	if o.VisionModels != nil {
		dst.VisionModels = o.VisionModels
	}
	if o.ThinkingLevels != nil {
		dst.ThinkingLevels = o.ThinkingLevels
	}
	if o.Headers != nil {
		if dst.Headers == nil {
			dst.Headers = map[string]string{}
		}
		maps.Copy(dst.Headers, o.Headers)
	}
	if o.MaxTokens != 0 {
		dst.MaxTokens = o.MaxTokens
	}
}

// ResolveModel turns a user reference into (provider, model). Accepted forms:
// "provider/model", "model" (searched across provider model lists, default
// provider first), "provider/" or "" (provider/default model).
func (s *Settings) ResolveModel(ref string) (ProviderConfig, string, error) {
	providers := s.ProviderConfigs()
	defProvider := s.DefaultProvider
	if defProvider == "" {
		defProvider = "opencode-go"
	}
	if ref == "" {
		ref = s.DefaultModel
		if ref != "" && !strings.Contains(ref, "/") {
			ref = defProvider + "/" + ref
		}
	}
	if ref == "" {
		ref = defProvider + "/"
	}

	if name, model, ok := strings.Cut(ref, "/"); ok {
		if pc, found := providers[name]; found {
			if model == "" {
				model = pc.DefaultModel
			}
			if model == "" {
				return pc, "", fmt.Errorf("provider %s has no default model; use --model %s/<model>", name, name)
			}
			return pc, model, nil
		}
		// Not a provider prefix: the model id itself may contain '/', as on
		// OpenRouter.
	}

	names := slices.Sorted(maps.Keys(providers))
	sort.SliceStable(names, func(i, j int) bool { return names[i] == defProvider && names[j] != defProvider })
	for _, name := range names {
		if slices.Contains(providers[name].Models, ref) {
			return providers[name], ref, nil
		}
	}
	return providers[defProvider], ref, nil
}

// ResolveAPIKey returns the configured key, reading the env var when set.
func (pc ProviderConfig) ResolveAPIKey() (string, error) {
	if pc.APIKey != "" {
		return os.ExpandEnv(pc.APIKey), nil
	}
	if pc.APIKeyEnv == "" {
		return "", nil // local endpoints
	}
	if k := os.Getenv(pc.APIKeyEnv); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("%s is not set (needed for provider %s); export it or add it to %s",
		pc.APIKeyEnv, pc.Name, filepath.Join(Home(), ".env"))
}

// NewProvider builds the runtime provider for pc and model.
func (pc ProviderConfig) NewProvider(model string) (agent.Provider, error) {
	key, err := pc.ResolveAPIKey()
	if err != nil {
		return nil, err
	}
	switch pc.API {
	case "", openai.API:
		return openai.New(openai.Config{
			Name:           pc.Name,
			BaseURL:        pc.BaseURL,
			APIKey:         key,
			Headers:        pc.Headers,
			SessionHeader:  pc.SessionHeader,
			UserAgent:      UserAgent,
			ThinkingFormat: pc.ThinkingFormat,
			MaxTokens:      pc.MaxTokens,
			SupportsImages: slices.Contains(pc.VisionModels, model),
		}), nil
	}
	return nil, fmt.Errorf("provider %s: unsupported api %q", pc.Name, pc.API)
}

// FetchModels lists the models the endpoint currently serves.
func (pc ProviderConfig) FetchModels(ctx context.Context) ([]string, error) {
	key, err := pc.ResolveAPIKey()
	if err != nil {
		return nil, err
	}
	switch pc.API {
	case "", openai.API:
		return openai.New(openai.Config{
			Name:      pc.Name,
			BaseURL:   pc.BaseURL,
			APIKey:    key,
			Headers:   pc.Headers,
			UserAgent: UserAgent,
		}).ListModels(ctx)
	}
	return nil, fmt.Errorf("provider %s: listing models is not supported for api %q", pc.Name, pc.API)
}

// ValidThinking normalizes a requested level against what the provider
// supports, falling back to its default.
func (pc ProviderConfig) ValidThinking(level string) string {
	if level != "" && (len(pc.ThinkingLevels) == 0 || slices.Contains(pc.ThinkingLevels, level)) {
		return level
	}
	if pc.DefaultThinking != "" {
		return pc.DefaultThinking
	}
	return "off"
}
