package rpc

import (
	"encoding/json"
	"math"
	"slices"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
)

// Wire shapes, matching what tau's RPC mode sends (rpc/testdata/tau_*.jsonl
// are recordings of it). Field order follows tau's so the output reads the
// same, though clients must not rely on order.

// response answers one command.
type response struct {
	Type    string `json:"type"` // always "response"
	Command string `json:"command"`
	Success bool   `json:"success"`
	ID      any    `json:"id,omitempty"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

type modelCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// modelWire describes a model the way Pi's frontends expect.
type modelWire struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	API           string    `json:"api"`
	Provider      string    `json:"provider"`
	BaseURL       string    `json:"baseUrl"`
	Reasoning     bool      `json:"reasoning"`
	Input         []string  `json:"input"`
	ContextWindow int       `json:"contextWindow"`
	MaxTokens     int       `json:"maxTokens"`
	Cost          modelCost `json:"cost"`
}

// defaultMaxTokens is tau's figure for a model that does not state one.
const defaultMaxTokens = 16_384

func modelOf(pc coding.ProviderConfig, model string) modelWire {
	api := pc.API
	if api == "" {
		api = "openai-completions"
	}
	input := []string{"text"}
	if slices.Contains(pc.VisionModels, model) {
		input = append(input, "image")
	}
	maxTokens := defaultMaxTokens
	if pc.MaxTokens > 0 {
		maxTokens = pc.MaxTokens
	}
	return modelWire{
		ID:            model,
		Name:          model,
		API:           api,
		Provider:      pc.Name,
		BaseURL:       pc.BaseURL,
		Reasoning:     len(pc.ThinkingLevels) > 1,
		Input:         input,
		ContextWindow: pc.ContextWindowTokens(),
		MaxTokens:     maxTokens,
	}
}

// trustWire reports project trust: whether the working directory's
// instruction files are loaded, and why. Paths only, never contents, the same
// rule as coding.TrustState.TrustNotice: printing them would put unvetted
// instructions in front of the user.
type trustWire struct {
	Decision      string   `json:"decision"` // "trusted" or "untrusted"
	Pending       bool     `json:"pending"`  // withheld only because nobody has decided
	Source        string   `json:"source"`
	Path          string   `json:"path"`
	InheritedFrom string   `json:"inheritedFrom,omitempty"`
	Files         []string `json:"files"`
	Total         int      `json:"total"`
}

func trustOf(t coding.TrustState) trustWire {
	files := t.Resources.Files
	if files == nil {
		files = []string{}
	}
	return trustWire{
		Decision:      string(t.Decision),
		Pending:       t.Pending,
		Source:        t.Source,
		Path:          t.Path,
		InheritedFrom: t.InheritedFrom,
		Files:         files,
		Total:         t.Resources.Total,
	}
}

// stateWire is get_state: tau's twelve keys plus projectTrust.
type stateWire struct {
	Model                 modelWire `json:"model"`
	ThinkingLevel         string    `json:"thinkingLevel"`
	IsStreaming           bool      `json:"isStreaming"`
	IsCompacting          bool      `json:"isCompacting"`
	SteeringMode          string    `json:"steeringMode"`
	FollowUpMode          string    `json:"followUpMode"`
	SessionFile           *string   `json:"sessionFile"`
	SessionID             string    `json:"sessionId"`
	SessionName           *string   `json:"sessionName"`
	AutoCompactionEnabled bool      `json:"autoCompactionEnabled"`
	MessageCount          int       `json:"messageCount"`
	PendingMessageCount   int       `json:"pendingMessageCount"`
	ProjectTrust          trustWire `json:"projectTrust"`
}

func stateOf(s *coding.Session) stateWire {
	st := s.State()
	pending := st.Steering + st.FollowUp
	if st.HeldPrompt != "" {
		pending++
	}
	var file *string
	if p := s.Path(); p != "" {
		file = &p
	}
	return stateWire{
		Model:                 modelOf(s.Provider(), s.Model()),
		ThinkingLevel:         s.ThinkingLevel(),
		IsStreaming:           st.Running,
		IsCompacting:          st.Compacting,
		SteeringMode:          "one-at-a-time",
		FollowUpMode:          "one-at-a-time",
		SessionFile:           file,
		SessionID:             s.Harness.Config().SessionID,
		AutoCompactionEnabled: s.AutoCompactionEnabled(),
		MessageCount:          len(s.Harness.Messages()),
		PendingMessageCount:   pending,
		ProjectTrust:          trustOf(s.TrustState()),
	}
}

type tokensWire struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Total      int64 `json:"total"`
}

type contextUsageWire struct {
	Tokens        int64   `json:"tokens"`
	ContextWindow int     `json:"contextWindow"`
	Percent       float64 `json:"percent"`
}

// statsWire is get_session_stats, summed over the transcript.
type statsWire struct {
	SessionFile       *string          `json:"sessionFile"`
	SessionID         string           `json:"sessionId"`
	UserMessages      int              `json:"userMessages"`
	AssistantMessages int              `json:"assistantMessages"`
	ToolCalls         int              `json:"toolCalls"`
	ToolResults       int              `json:"toolResults"`
	TotalMessages     int              `json:"totalMessages"`
	Tokens            tokensWire       `json:"tokens"`
	Cost              float64          `json:"cost"`
	ContextUsage      contextUsageWire `json:"contextUsage"`
}

func statsOf(s *coding.Session) statsWire {
	msgs := s.Harness.Messages()
	out := statsWire{SessionID: s.Harness.Config().SessionID, TotalMessages: len(msgs)}
	if p := s.Path(); p != "" {
		out.SessionFile = &p
	}
	for _, m := range msgs {
		switch v := m.(type) {
		case *agent.UserMessage:
			out.UserMessages++
		case *agent.AssistantMessage:
			out.AssistantMessages++
			out.ToolCalls += len(v.ToolCalls())
			u := v.Usage
			out.Tokens.Input += u.Input
			out.Tokens.Output += u.Output
			out.Tokens.CacheRead += u.CacheRead
			out.Tokens.CacheWrite += u.CacheWrite
			out.Tokens.Total += u.TotalTokens
			out.Cost += u.Cost.Total
		case *agent.ToolResultMessage:
			out.ToolResults++
		}
	}
	ctx := s.ContextStats()
	window := s.ContextWindow()
	out.ContextUsage = contextUsageWire{Tokens: ctx.EffectivePrompt(), ContextWindow: window}
	if window > 0 {
		out.ContextUsage.Percent = math.Round(float64(ctx.EffectivePrompt())/float64(window)*10000) / 100
	}
	return out
}

// eventJSON encodes one event from Session.Subscribe. agent_end gains the
// willRetry field that tau's session-level agent_end carries; malachi never
// retries a finished run, so it is always false.
func eventJSON(e any) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if _, ok := e.(*agent.AgentEndEvent); ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		fields["willRetry"] = json.RawMessage("false")
		return json.Marshal(fields)
	}
	return raw, nil
}
