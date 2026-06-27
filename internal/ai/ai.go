// Package ai is loft.ai: a server-keyed chat-completions proxy. Hosted apps POST messages; loftd
// calls the model through an LLMAdapter (no key in the browser) and returns the reply, streaming or
// not. The model is server-chosen. Cost is bounded five ways: the provider's TPM cap upstream, a max
// output-token cap, oversized-prompt rejection, a per-user rate limit, and a per-site daily token
// budget. NOTE: the rate limit and budget are per-process, so they assume a single instance.
//
// loftd mints the OpenAI chat-completions shape itself (a completion object, or chunk SSE ending in
// [DONE]) rather than passing the provider's bytes through, so the consumer contract is the same
// regardless of which upstream API or provider the adapter calls.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/larsakerlund/loft/internal/config"
	"github.com/larsakerlund/loft/internal/limit"
	"github.com/larsakerlund/loft/internal/web"
)

// Request bounds. No overall client timeout (streaming is long-lived); the context bounds the call.
const (
	nonStreamTimeout = 60 * time.Second
	streamTimeout    = 10 * time.Minute
)

const (
	maxInputBytes        = 16_000  // cap on the marshaled messages (bytes, not chars)
	reqsPerMin           = 20      // per-(site,user) sustained request rate
	reqBurst             = 5       // per-(site,user) burst allowance
	dailyTokensPerSite   = 500_000 // per-site daily token budget
	dailyTokensPerUser   = 200_000 // per-(site,user) daily sub-cap, so one user can't drain the site
	budgetEvictThreshold = 10_000  // sweep stale day buckets once the usage map exceeds this
	maxConcurrentChats   = 64      // global in-flight chats (bounds upstream conns/goroutines/memory)
)

// Service is the loft.ai HTTP service.
type Service struct {
	cfg        config.Config
	adapter    LLMAdapter
	adapterErr error

	reqs *limit.Limiter // request rate limit, keyed per (site,user)
	sem  chan struct{}  // global concurrency cap on in-flight chats

	usageMu   sync.Mutex
	usage     map[string]*dayUsage // per-site daily token usage
	userUsage map[string]*dayUsage // per-(site,user) daily token usage
}

// New builds the service.
func New(cfg config.Config) *Service {
	s := &Service{
		cfg:       cfg,
		reqs:      limit.New(reqsPerMin, reqBurst),
		sem:       make(chan struct{}, maxConcurrentChats),
		usage:     map[string]*dayUsage{},
		userUsage: map[string]*dayUsage{},
	}
	s.adapter, s.adapterErr = newAdapter(cfg)
	return s
}

// Init warms the identity token at startup (a no-op on the api-key path) so the first chat isn't
// slowed by an auth round-trip.
func (s *Service) Init(ctx context.Context) error {
	if s.adapter == nil {
		return nil
	}
	if w, ok := s.adapter.(interface {
		warm(context.Context) error
	}); ok {
		return w.warm(ctx)
	}
	return nil
}

// Handler serves POST /api/ai/chat.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			web.Error(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.chat(w, r)
	})
}

// chatBody is the decoded request payload for POST /api/ai/chat.
type chatBody struct {
	Messages []json.RawMessage `json:"messages"`
	Stream   bool              `json:"stream"`
	parsed   []Message         // role/content extracted and validated by parseChatRequest
}

func (s *Service) chat(w http.ResponseWriter, r *http.Request) {
	if s.adapter == nil {
		if s.adapterErr != nil {
			log.Printf("loftd ai adapter unavailable: %v", s.adapterErr)
			web.Error(w, http.StatusInternalServerError, "ai error")
			return
		}
		web.Error(w, http.StatusNotImplemented, "ai not configured")
		return
	}
	user, _ := web.User(r.Context())
	site := web.Site(r)

	// Validate the request BEFORE consuming any limiter, so malformed input can't burn a caller's
	// rate budget or a site's token budget.
	body, ok := parseChatRequest(w, r)
	if !ok {
		return
	}
	if !s.allowReq(site, user.ID) {
		web.Error(w, http.StatusTooManyRequests, "ai rate limit — slow down")
		return
	}
	// Global concurrency cap: bound in-flight upstream calls (and the goroutines/sockets/memory they
	// hold) regardless of how many distinct callers pass the per-user rate limit. Non-blocking: shed
	// load with 503 rather than queueing requests behind a long-lived stream.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		web.Error(w, http.StatusServiceUnavailable, "ai busy, try again")
		return
	}
	reserved, day, ok := s.reserve(site, user.ID, body)
	if !ok {
		web.Error(w, http.StatusTooManyRequests, "ai daily budget reached")
		return
	}

	timeout := nonStreamTimeout
	if body.Stream {
		timeout = streamTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	c := charge{site: site, userID: user.ID, day: day, reserved: reserved}
	if body.Stream {
		s.streamChat(ctx, w, body.parsed, c)
		return
	}
	s.completeChat(ctx, w, body.parsed, c)
}

// charge carries the budget reservation context an in-flight response must reconcile against.
type charge struct {
	site, userID string
	day          int64
	reserved     int
}

var validRoles = map[string]bool{"system": true, "user": true, "assistant": true}

// parseChatRequest reads, decodes, and fully validates the chat request body (including each
// message's role/content) so a malformed shape is rejected here (400) rather than wasting an
// upstream round-trip and surfacing as a confusing 502. On success it fills body.parsed with the
// validated turns. Writes the error and returns ok=false otherwise.
func parseChatRequest(w http.ResponseWriter, r *http.Request) (chatBody, bool) {
	const badReq = "messages: [{ role, content }] required"
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, maxInputBytes*2)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			web.Error(w, http.StatusRequestEntityTooLarge, "prompt too large")
		} else {
			web.Error(w, http.StatusBadRequest, badReq)
		}
		return body, false
	}
	if json.Unmarshal(data, &body) != nil || len(body.Messages) == 0 {
		web.Error(w, http.StatusBadRequest, badReq)
		return body, false
	}
	body.parsed = make([]Message, 0, len(body.Messages))
	for _, raw := range body.Messages {
		var m struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil || !validRoles[m.Role] || strings.TrimSpace(m.Content) == "" {
			web.Error(w, http.StatusBadRequest, badReq)
			return body, false
		}
		body.parsed = append(body.parsed, Message{Role: m.Role, Content: m.Content})
	}
	if msgs, _ := json.Marshal(body.Messages); len(msgs) > maxInputBytes {
		web.Error(w, http.StatusRequestEntityTooLarge, "prompt too large")
		return body, false
	}
	return body, true
}

// completeChat runs a non-streaming completion and writes a chat.completion object.
func (s *Service) completeChat(ctx context.Context, w http.ResponseWriter, msgs []Message, c charge) {
	comp, err := s.adapter.Complete(ctx, msgs)
	if err != nil {
		s.refund(c.site, c.userID, c.day, c.reserved) // failed upstream call produced no billable output
		s.writeUpstreamError(w, err)
		return
	}
	// An empty reply that did not stop normally (no choices, content filter, truncation) is an
	// upstream failure, not a valid empty answer: surface it as 502 rather than a successful "".
	if comp.Content == "" && comp.FinishReason != "stop" {
		s.refund(c.site, c.userID, c.day, c.reserved)
		web.Error(w, http.StatusBadGateway, "ai error")
		return
	}
	// Reconcile to actual usage only when upstream reported it; a reply that omits/zeroes usage keeps
	// the worst-case reservation so an abnormal response can't refund free tokens.
	if comp.Usage.TotalTokens > 0 {
		s.reconcile(c.site, c.userID, c.day, c.reserved, comp.Usage.TotalTokens)
	}
	web.JSON(w, http.StatusOK, completionObject(s.cfg.AIModelName(), comp))
}

// streamChat runs a streaming completion and writes chat.completion.chunk SSE ending in [DONE]. A
// truncated upstream stream is reported by withholding the [DONE] terminator (the consumer detects
// the missing terminator), so a partial reply can't look complete. Usage is reconciled to real
// tokens only when the stream reported them; an abort keeps the full reservation.
func (s *Service) streamChat(ctx context.Context, w http.ResponseWriter, msgs []Message, c charge) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // tell the reverse proxy not to buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id, created, model := "chatcmpl-"+uuid.NewString(), time.Now().Unix(), s.cfg.AIModelName()
	writeChunk := func(delta, finish string, usage *Usage) {
		b, _ := json.Marshal(chunkObject(id, created, model, delta, finish, usage))
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	usage, finish, err := s.adapter.Stream(ctx, msgs, func(delta string) error {
		writeChunk(delta, "", nil)
		return nil
	})
	if usage.TotalTokens > 0 {
		s.reconcile(c.site, c.userID, c.day, c.reserved, usage.TotalTokens)
	}
	if err != nil {
		// A failure before any tokens streamed produced no billable output: refund (matching the
		// non-streaming path). A mid-stream abort keeps the reservation so it can't yield free tokens.
		if usage.TotalTokens == 0 {
			s.refund(c.site, c.userID, c.day, c.reserved)
		}
		log.Printf("loftd ai stream interrupted (upstream status=%d)", upstreamStatus(err))
		return // no [DONE]: the consumer sees the missing terminator as a truncated reply
	}
	if finish == "" {
		finish = "stop"
	}
	writeChunk("", finish, &usage) // final chunk: finish reason + usage, then the terminator
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// writeUpstreamError maps a provider error to an HTTP status: a 429 is surfaced as 429 (the caller
// can back off), anything else as 502. The upstream body is never echoed (it may carry detail).
func (s *Service) writeUpstreamError(w http.ResponseWriter, err error) {
	status := upstreamStatus(err)
	log.Printf("loftd ai upstream error (status=%d)", status)
	if status == http.StatusTooManyRequests {
		web.Error(w, http.StatusTooManyRequests, "ai busy, try again")
		return
	}
	web.Error(w, http.StatusBadGateway, "ai error")
}

// completionObject mints a chat.completion from the adapter reply (loft owns the shape; the model is
// loft's configured model name, not whatever the provider returned).
func completionObject(model string, c Completion) map[string]any {
	finish := c.FinishReason
	if finish == "" {
		finish = "stop"
	}
	return map[string]any{
		"id":      "chatcmpl-" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": c.Content},
			"finish_reason": finish,
		}},
		"usage": usageObject(c.Usage),
	}
}

// chunkObject mints a chat.completion.chunk. An empty delta yields an empty delta object, an empty
// finish yields a null finish_reason, and usage is included only when non-nil (the final chunk).
func chunkObject(id string, created int64, model, delta, finish string, usage *Usage) map[string]any {
	deltaObj := map[string]any{}
	if delta != "" {
		deltaObj["content"] = delta
	}
	var finishVal any
	if finish != "" {
		finishVal = finish
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "delta": deltaObj, "finish_reason": finishVal}},
	}
	if usage != nil {
		chunk["usage"] = usageObject(*usage)
	}
	return chunk
}

func usageObject(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}
