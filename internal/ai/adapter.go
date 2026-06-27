// The upstream LLM seam. LLMAdapter abstracts the provider call so the service layer (rate limits,
// daily budgets, and the client-facing chat-completions format) stays provider-neutral. The format
// loftd emits to the browser is minted by the service, not passed through, so the consumer contract
// is independent of whichever upstream API or provider this calls.
package ai

import (
	"context"
	"errors"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/larsakerlund/loft/internal/config"
)

const aiScope = "https://cognitiveservices.azure.com/.default"

// Message is one chat turn.
type Message struct{ Role, Content string }

// Usage is the token accounting a provider reports (zero fields when it reports none).
type Usage struct{ PromptTokens, CompletionTokens, TotalTokens int }

// Completion is a non-streaming reply.
type Completion struct {
	Content      string
	FinishReason string
	Usage        Usage
}

// LLMAdapter performs a chat completion. Implementations own the wire format and auth; the service
// layers limits/budgets and the consumer-facing format on top.
type LLMAdapter interface {
	Complete(ctx context.Context, msgs []Message) (Completion, error)
	// Stream invokes onDelta per content token and returns the final usage and finish reason.
	Stream(ctx context.Context, msgs []Message, onDelta func(string) error) (Usage, string, error)
}

// upstreamError carries the provider's HTTP status so the service can map it (429 -> 429, else 502).
type upstreamError struct {
	status int
	err    error
}

func (e *upstreamError) Error() string { return e.err.Error() }
func (e *upstreamError) Unwrap() error { return e.err }

// upstreamStatus returns the provider HTTP status for an adapter error, or 0 if transport-level.
func upstreamStatus(err error) int {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.status
	}
	return 0
}

// newAdapter builds the upstream adapter from config, or returns (nil, nil) when AI is unconfigured.
// Two modes off the same openai-go client: set LOFT_AI_DEPLOYMENT for Azure OpenAI / AI Foundry
// (deployment in the URL, api-version query, Azure key or managed identity), or set LOFT_AI_MODEL for
// any OpenAI-compatible endpoint (OpenAI, Groq, Together, vLLM, Ollama, LiteLLM: base URL + API key,
// model in the body, no Azure).
func newAdapter(cfg config.Config) (LLMAdapter, error) {
	model := cfg.AIModelName()
	if cfg.AIEndpoint == "" || model == "" {
		return nil, nil
	}
	// No SDK-level retries: loftd has its own rate limiting and per-call token budget, so a silent
	// upstream retry would add latency and risk double-charging a reservation.
	opts := []option.RequestOption{option.WithMaxRetries(0)}
	var cred azcore.TokenCredential

	if cfg.AIDeployment != "" {
		opts = append(opts, azure.WithEndpoint(cfg.AIEndpoint, cfg.AIAPIVersion))
		if cfg.AIKey != "" {
			opts = append(opts, azure.WithAPIKey(cfg.AIKey))
		} else {
			var mopts *azidentity.ManagedIdentityCredentialOptions
			if cfg.UAMIClientID != "" {
				mopts = &azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(cfg.UAMIClientID)}
			}
			c, err := azidentity.NewManagedIdentityCredential(mopts)
			if err != nil {
				return nil, err
			}
			cred = c
			opts = append(opts, azure.WithTokenCredential(c))
		}
	} else {
		opts = append(opts, option.WithBaseURL(cfg.AIEndpoint))
		if cfg.AIKey != "" {
			opts = append(opts, option.WithAPIKey(cfg.AIKey))
		}
	}

	client := openai.NewClient(opts...)
	return &openaiAdapter{
		client:          client,
		cred:            cred,
		model:           model,
		maxTokens:       int64(cfg.AIMaxTokens),
		reasoningEffort: cfg.AIReasoningEffort,
	}, nil
}

type openaiAdapter struct {
	client          openai.Client
	cred            azcore.TokenCredential // nil on the api-key path; warmed at boot when set
	model           string
	maxTokens       int64
	reasoningEffort string
}

func (a *openaiAdapter) params(msgs []Message) openai.ChatCompletionNewParams {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system":
			out = append(out, openai.SystemMessage(m.Content))
		case "assistant":
			out = append(out, openai.AssistantMessage(m.Content))
		default:
			out = append(out, openai.UserMessage(m.Content))
		}
	}
	p := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(a.model),
		Messages:            out,
		MaxCompletionTokens: openai.Int(a.maxTokens),
	}
	if a.reasoningEffort != "" {
		p.ReasoningEffort = shared.ReasoningEffort(a.reasoningEffort)
	}
	return p
}

func (a *openaiAdapter) Complete(ctx context.Context, msgs []Message) (Completion, error) {
	resp, err := a.client.Chat.Completions.New(ctx, a.params(msgs))
	if err != nil {
		return Completion{}, wrapErr(err)
	}
	c := Completion{Usage: Usage{
		PromptTokens:     int(resp.Usage.PromptTokens),
		CompletionTokens: int(resp.Usage.CompletionTokens),
		TotalTokens:      int(resp.Usage.TotalTokens),
	}}
	if len(resp.Choices) > 0 {
		c.Content = resp.Choices[0].Message.Content
		c.FinishReason = string(resp.Choices[0].FinishReason)
	}
	return c, nil
}

func (a *openaiAdapter) Stream(ctx context.Context, msgs []Message, onDelta func(string) error) (Usage, string, error) {
	p := a.params(msgs)
	p.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	stream := a.client.Chat.Completions.NewStreaming(ctx, p)
	var usage Usage
	var finish string
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) > 0 {
			if d := chunk.Choices[0].Delta.Content; d != "" {
				if err := onDelta(d); err != nil {
					return usage, finish, err
				}
			}
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				finish = string(fr)
			}
		}
		if chunk.Usage.TotalTokens > 0 {
			usage = Usage{
				PromptTokens:     int(chunk.Usage.PromptTokens),
				CompletionTokens: int(chunk.Usage.CompletionTokens),
				TotalTokens:      int(chunk.Usage.TotalTokens),
			}
		}
	}
	if err := stream.Err(); err != nil {
		return usage, finish, wrapErr(err)
	}
	return usage, finish, nil
}

// warm fetches an identity token at boot so the first chat doesn't pay the auth round-trip. It is a
// no-op on the api-key path.
func (a *openaiAdapter) warm(ctx context.Context) error {
	if a.cred == nil {
		return nil
	}
	_, err := a.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{aiScope}})
	return err
}

// wrapErr classifies an openai-go error so the service can map the provider status to an HTTP status.
func wrapErr(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &upstreamError{status: apiErr.StatusCode, err: err}
	}
	return err
}
