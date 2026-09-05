package catalog

type Type string

const (
	TypeOpenAI       Type = "openai"
	TypeOpenAICompat Type = "openai-compat"
	TypeOpenRouter   Type = "openrouter"
	TypeVercel       Type = "vercel"
	TypeAnthropic    Type = "anthropic"
	TypeGoogle       Type = "google"
	TypeAzure        Type = "azure"
	TypeBedrock      Type = "bedrock"
	TypeVertexAI     Type = "google-vertex"
)

type ProviderID string

const (
	ProviderOpenAI           ProviderID = "openai"
	ProviderAnthropic        ProviderID = "anthropic"
	ProviderSynthetic        ProviderID = "synthetic"
	ProviderGemini           ProviderID = "gemini"
	ProviderAzure            ProviderID = "azure"
	ProviderBedrock          ProviderID = "bedrock"
	ProviderBedrockEurope    ProviderID = "bedrock-europe"
	ProviderVertexAI         ProviderID = "vertexai"
	ProviderXAI              ProviderID = "xai"
	ProviderZAI              ProviderID = "zai"
	ProviderDeepSeek         ProviderID = "deepseek"
	ProviderZhipu            ProviderID = "zhipu"
	ProviderZhipuCoding      ProviderID = "zhipu-coding"
	ProviderGROQ             ProviderID = "groq"
	ProviderOpenRouter       ProviderID = "openrouter"
	ProviderCerebras         ProviderID = "cerebras"
	ProviderVenice           ProviderID = "venice"
	ProviderChutes           ProviderID = "chutes"
	ProviderHuggingFace      ProviderID = "huggingface"
	ProviderAIHubMix         ProviderID = "aihubmix"
	ProviderKimiCoding       ProviderID = "kimi-coding"
	ProviderCopilot          ProviderID = "copilot"
	ProviderCortecs          ProviderID = "cortecs"
	ProviderVercel           ProviderID = "vercel"
	ProviderMiniMax          ProviderID = "minimax"
	ProviderMiniMaxChina     ProviderID = "minimax-china"
	ProviderIoNet            ProviderID = "ionet"
	ProviderQiniuCloud       ProviderID = "qiniucloud"
	ProviderAvian            ProviderID = "avian"
	ProviderNebius           ProviderID = "nebius"
	ProviderNeuralwatt       ProviderID = "neuralwatt"
	ProviderOpenCodeZen      ProviderID = "opencode-zen"
	ProviderOpenCodeGo       ProviderID = "opencode-go"
	ProviderAlibabaSingapore ProviderID = "alibaba-singapore"
	ProviderAlibabaUS        ProviderID = "alibaba-us"
	ProviderFireworks        ProviderID = "fireworks"
	ProviderBaseten          ProviderID = "baseten"
	ProviderMoonshot         ProviderID = "moonshot"
	ProviderAtlasCloud       ProviderID = "atlascloud"
)

type Provider struct {
	Name                string            `json:"name"`
	ID                  ProviderID        `json:"id"`
	APIKey              string            `json:"api_key,omitempty"`
	APIEndpoint         string            `json:"api_endpoint,omitempty"`
	Type                Type              `json:"type,omitempty"`
	DefaultLargeModelID string            `json:"default_large_model_id,omitempty"`
	DefaultSmallModelID string            `json:"default_small_model_id,omitempty"`
	Models              []Model           `json:"models,omitempty"`
	DefaultHeaders      map[string]string `json:"default_headers,omitempty"`
}

type ModelOptions struct {
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	TopK             *int64         `json:"top_k,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	ProviderOptions  map[string]any `json:"provider_options,omitempty"`
}

type Model struct {
	ID                     string       `json:"id"`
	Name                   string       `json:"name"`
	CostPer1MIn            float64      `json:"cost_per_1m_in"`
	CostPer1MOut           float64      `json:"cost_per_1m_out"`
	CostPer1MInCached      float64      `json:"cost_per_1m_in_cached"`
	CostPer1MOutCached     float64      `json:"cost_per_1m_out_cached"`
	ContextWindow          int64        `json:"context_window"`
	DefaultMaxTokens       int64        `json:"default_max_tokens"`
	CanReason              bool         `json:"can_reason"`
	ReasoningLevels        []string     `json:"reasoning_levels,omitempty"`
	DefaultReasoningEffort string       `json:"default_reasoning_effort,omitempty"`
	SupportsImages         bool         `json:"supports_attachments"`
	Options                ModelOptions `json:"options,omitzero"`
}

func KnownProviders() []ProviderID {
	return []ProviderID{
		ProviderOpenAI,
		ProviderSynthetic,
		ProviderAnthropic,
		ProviderGemini,
		ProviderAzure,
		ProviderBedrock,
		ProviderBedrockEurope,
		ProviderVertexAI,
		ProviderXAI,
		ProviderZAI,
		ProviderZhipu,
		ProviderZhipuCoding,
		ProviderGROQ,
		ProviderOpenRouter,
		ProviderCerebras,
		ProviderVenice,
		ProviderChutes,
		ProviderHuggingFace,
		ProviderAIHubMix,
		ProviderKimiCoding,
		ProviderCopilot,
		ProviderCortecs,
		ProviderVercel,
		ProviderMiniMax,
		ProviderMiniMaxChina,
		ProviderQiniuCloud,
		ProviderAvian,
		ProviderNebius,
		ProviderNeuralwatt,
		ProviderOpenCodeZen,
		ProviderOpenCodeGo,
		ProviderFireworks,
		ProviderBaseten,
		ProviderMoonshot,
		ProviderAtlasCloud,
	}
}

func KnownProviderTypes() []Type {
	return []Type{
		TypeOpenAI,
		TypeOpenAICompat,
		TypeOpenRouter,
		TypeVercel,
		TypeAnthropic,
		TypeGoogle,
		TypeAzure,
		TypeBedrock,
		TypeVertexAI,
	}
}
