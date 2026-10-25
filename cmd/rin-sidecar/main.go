package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	rin "github.com/rinspacehq/rinspace-rin"
)

const maxRequestBytes = 128 << 10

type provider interface {
	Complete(context.Context, rin.CompletionRequest) (rin.Completion, error)
	ReadReceipt(context.Context, string) (rin.OperationReceipt, error)
	AbortReceipt(context.Context, string) (rin.OperationReceipt, error)
	CompleteAgent(context.Context, rin.AgentCompletionRequest) (rin.AgentCompletion, error)
	ReadAgentReceipt(context.Context, string) (rin.AgentOperationReceipt, error)
	AbortAgentReceipt(context.Context, string) (rin.AgentOperationReceipt, error)
}

type sidecar struct {
	token     string
	provider  provider
	model     string
	maxTokens int
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, code string) {
	reply(w, status, struct {
		Code string `json:"code"`
	}{Code: code})
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) error {
	return readJSONLimit(w, r, out, maxRequestBytes)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	if r.Header.Get("Content-Type") != "application/json" || r.ContentLength > limit {
		return errors.New("invalid content type or length")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func (s sidecar) authorized(r *http.Request) bool {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(value, "Bearer ")
	return len(provided) == len(s.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

func (s sidecar) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/prepare", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			problem(w, http.StatusUnauthorized, "UNAUTHENTICATED")
			return
		}
		var input rin.ChatInput
		if readJSON(w, r, &input) != nil || input.OperationID != "" || len(input.History) == 0 {
			problem(w, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		prepared := rin.CompletionRequest{Model: s.model, Messages: rin.BuildMessages(input), MaxTokens: s.maxTokens}
		reply(w, http.StatusOK, prepared)
	})
	mux.HandleFunc("POST /v1/complete", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			problem(w, http.StatusUnauthorized, "UNAUTHENTICATED")
			return
		}
		var input struct {
			OperationID string          `json:"operation_id"`
			Frozen      json.RawMessage `json:"frozen"`
		}
		if readJSON(w, r, &input) != nil || input.OperationID == "" || len(input.Frozen) == 0 {
			problem(w, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		var frozen rin.CompletionRequest
		if json.Unmarshal(input.Frozen, &frozen) != nil || frozen.OperationID != "" ||
			frozen.Model != rin.DefaultModel || frozen.MaxTokens < 1 || frozen.MaxTokens > 4096 || len(frozen.Messages) < 2 {
			problem(w, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		frozen.OperationID = input.OperationID
		completion, err := s.provider.Complete(r.Context(), frozen)
		if err != nil {
			problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
			return
		}
		completion.Text = rin.CleanReply(completion.Text)
		if utf8.RuneCountInString(completion.Text) < 2 {
			problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
			return
		}
		reply(w, http.StatusOK, completion)
	})
	mux.HandleFunc("POST /v1/agent/completions", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			problem(w, http.StatusUnauthorized, "UNAUTHENTICATED")
			return
		}
		var input struct {
			OperationID     string                    `json:"operation_id"`
			Messages        []rin.AgentMessage        `json:"messages"`
			Tools           []rin.AgentToolDefinition `json:"tools"`
			MaxOutputTokens int                       `json:"max_output_tokens"`
		}
		if readJSONLimit(w, r, &input, rin.MaxAgentRequestBytes) != nil ||
			input.MaxOutputTokens > s.maxTokens {
			problem(w, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		request := rin.AgentCompletionRequest{
			OperationID: input.OperationID, Model: s.model, Messages: input.Messages,
			Tools: input.Tools, MaxTokens: input.MaxOutputTokens,
		}
		if rin.ValidateAgentRequest(request) != nil {
			problem(w, http.StatusBadRequest, "INVALID_REQUEST")
			return
		}
		completion, err := s.provider.CompleteAgent(r.Context(), request)
		if err != nil || rin.ValidateAgentCompletion(completion, request) != nil {
			problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
			return
		}
		reply(w, http.StatusOK, completion)
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.receipt(w, r, false)
	})
	mux.HandleFunc("POST /v1/operations/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		s.receipt(w, r, true)
	})
	mux.HandleFunc("GET /v1/agent/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.agentReceipt(w, r, false)
	})
	mux.HandleFunc("POST /v1/agent/operations/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		s.agentReceipt(w, r, true)
	})
	return mux
}

func (s sidecar) agentReceipt(w http.ResponseWriter, r *http.Request, abort bool) {
	if !s.authorized(r) {
		problem(w, http.StatusUnauthorized, "UNAUTHENTICATED")
		return
	}
	if r.ContentLength > 0 {
		problem(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	id := r.PathValue("id")
	var result rin.AgentOperationReceipt
	var err error
	if abort {
		result, err = s.provider.AbortAgentReceipt(r.Context(), id)
	} else {
		result, err = s.provider.ReadAgentReceipt(r.Context(), id)
	}
	if errors.Is(err, rin.ErrReceiptNotFound) {
		problem(w, http.StatusNotFound, "NOT_FOUND")
		return
	}
	if err != nil {
		problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
		return
	}
	status := http.StatusOK
	switch result.Status {
	case rin.ReceiptUnknown:
		status = http.StatusAccepted
	case rin.ReceiptExpired:
		status = http.StatusGone
	case rin.ReceiptCompleted, rin.ReceiptFailed:
	default:
		problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
		return
	}
	reply(w, status, result)
}

func (s sidecar) receipt(w http.ResponseWriter, r *http.Request, abort bool) {
	if !s.authorized(r) {
		problem(w, http.StatusUnauthorized, "UNAUTHENTICATED")
		return
	}
	if r.ContentLength > 0 {
		problem(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	id := r.PathValue("id")
	var result rin.OperationReceipt
	var err error
	if abort {
		result, err = s.provider.AbortReceipt(r.Context(), id)
	} else {
		result, err = s.provider.ReadReceipt(r.Context(), id)
	}
	if errors.Is(err, rin.ErrReceiptNotFound) {
		problem(w, http.StatusNotFound, "NOT_FOUND")
		return
	}
	if err != nil {
		problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
		return
	}
	status := http.StatusOK
	switch result.Status {
	case rin.ReceiptUnknown:
		status = http.StatusAccepted
	case rin.ReceiptExpired:
		status = http.StatusGone
	case rin.ReceiptCompleted:
		result.Completion.Text = rin.CleanReply(result.Completion.Text)
		if utf8.RuneCountInString(result.Completion.Text) < 2 {
			problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
			return
		}
	case rin.ReceiptFailed:
	default:
		problem(w, http.StatusBadGateway, "GATEWAY_UNAVAILABLE")
		return
	}
	reply(w, status, result)
}

func main() {
	token := os.Getenv("RIN_SIDECAR_TOKEN")
	endpoint := os.Getenv("RIN_GATEWAY_ENDPOINT")
	key := os.Getenv("RIN_GATEWAY_API_KEY")
	address := os.Getenv("RIN_SIDECAR_ADDR")
	if address == "" {
		address = ":8098"
	}
	maxTokens := 1200
	if raw := os.Getenv("RIN_MAX_TOKENS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 4096 {
			log.Fatal("Rin maximum output tokens is invalid")
		}
		maxTokens = parsed
	}
	if len(token) < 32 || strings.TrimSpace(endpoint) == "" || strings.TrimSpace(key) == "" {
		log.Fatal("Rin sidecar credential or gateway configuration is missing")
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Host == "" || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") ||
		parsedEndpoint.User != nil || parsedEndpoint.Fragment != "" || parsedEndpoint.RawQuery != "" || parsedEndpoint.Path != "/v1/chat/completions" {
		log.Fatal("Rin gateway endpoint must be a chat completions URL without credentials or query")
	}
	client := &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("gateway redirects are disabled")
	}}
	service := sidecar{token: token, provider: rin.OpenAIProvider{Endpoint: endpoint, APIKey: key, Client: client},
		model: rin.DefaultModel, maxTokens: maxTokens}
	server := &http.Server{Addr: address, Handler: service.handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 125 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 8 << 10}
	log.Printf("Rin sidecar listening on %s", address)
	log.Fatal(server.ListenAndServe())
}
