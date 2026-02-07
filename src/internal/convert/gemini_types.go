package convert

type GeminiGenerateContentRequest struct {
	SystemInstruction *GeminiContent        `json:"systemInstruction,omitempty"`
	Contents          []GeminiContent       `json:"contents"`
	GenerationConfig  *GeminiGenConfig      `json:"generationConfig,omitempty"`
	SafetySettings    []map[string]any      `json:"safetySettings,omitempty"`
	Tools             []map[string]any      `json:"tools,omitempty"`
	ToolConfig        map[string]any        `json:"toolConfig,omitempty"`
}

type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

type GeminiPart struct {
	Text string `json:"text,omitempty"`
}

type GeminiGenConfig struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
	MaxTokens   *int     `json:"maxOutputTokens,omitempty"`
}

type GeminiGenerateContentResponse struct {
	Candidates    []GeminiCandidate `json:"candidates"`
	UsageMetadata *GeminiUsage      `json:"usageMetadata,omitempty"`
}

type GeminiCandidate struct {
	Content GeminiContent `json:"content"`
}

type GeminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

