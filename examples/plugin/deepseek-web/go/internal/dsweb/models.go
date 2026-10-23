package dsweb

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Models is the catalog this plugin registers. The site exposes one model with
// two switches (reasoning and web search); the requested id is where a client's
// choice survives, so the ids below are the API surface and the executor
// derives the switches from them.
//
// IDs are registered bare. The host applies an auth prefix itself
// (applyModelPrefixes), so registering a prefixed id here would produce
// "prefix/prefix/model"; the executor strips a "deepseek-web/" prefix when a
// client sends one anyway.
func Models() []pluginapi.ModelInfo {
	base := func(id, name string, thinking, search bool) pluginapi.ModelInfo {
		description := "DeepSeek web chat"
		switch {
		case thinking && search:
			description = "DeepSeek web chat with reasoning and web search"
		case thinking:
			description = "DeepSeek web chat with reasoning"
		case search:
			description = "DeepSeek web chat with web search"
		}
		return pluginapi.ModelInfo{
			ID:                         id,
			Object:                     "model",
			OwnedBy:                    Provider,
			Type:                       "chat",
			DisplayName:                name,
			Description:                description,
			InputTokenLimit:            128000,
			OutputTokenLimit:           65536,
			ContextLength:              128000,
			MaxCompletionTokens:        65536,
			SupportedGenerationMethods: []string{"chat.completions"},
			SupportedParameters: []string{
				"messages", "stream", "temperature", "top_p", "max_tokens", "tools", "tool_choice",
			},
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		}
	}

	return []pluginapi.ModelInfo{
		base("deepseek-v4-pro", "DeepSeek V4 Pro", false, false),
		base("deepseek-v4-pro-think", "DeepSeek V4 Pro Think", true, false),
		base("deepseek-v4-pro-search", "DeepSeek V4 Pro Search", false, true),
		base("deepseek-v4-pro-think-search", "DeepSeek V4 Pro Think Search", true, true),
		base("deepseek-v4-flash", "DeepSeek V4 Flash", false, false),
		base("deepseek-v4-flash-think", "DeepSeek V4 Flash Think", true, false),
		base("deepseek-v4-flash-search", "DeepSeek V4 Flash Search", false, true),
		base("deepseek-v4-flash-think-search", "DeepSeek V4 Flash Think Search", true, true),
		base("deepseek-chat", "DeepSeek Chat", false, false),
		base("deepseek-reasoner", "DeepSeek Reasoner", true, false),
	}
}

// NormalizeModelID accepts both the bare registered id and a namespaced one.
func NormalizeModelID(model string) string {
	model = strings.TrimSpace(model)
	if len(model) > len(Provider) &&
		model[len(Provider)] == '/' &&
		strings.EqualFold(model[:len(Provider)], Provider) {
		return model[len(Provider)+1:]
	}
	return model
}
