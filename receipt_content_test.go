package rin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOpenAIProviderPurgeLearningReceiptPreservesUncertainStatus(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "Bearer synthetic-service-key" ||
			r.Header.Get("X-Rinspace-Budget-Profile") != ReaderLearningBudgetV1 || r.ContentLength != 0 {
			t.Error("cleanup must be an authenticated, body-free learning control request")
		}
		switch r.URL.Path {
		case "/v1/rin/operations/ready/content":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/rin/operations/pending/content":
			w.WriteHeader(http.StatusConflict)
		case "/v1/rin/operations/missing/content":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	provider := OpenAIProvider{Endpoint: server.URL + "/v1/chat/completions", APIKey: "synthetic-service-key", Client: server.Client()}
	if err := provider.PurgeLearningReceipt(context.Background(), "ready"); err != nil {
		t.Fatal(err)
	}
	if err := provider.PurgeLearningReceipt(context.Background(), "pending"); !errors.Is(err, ErrReceiptPending) {
		t.Fatalf("unknown receipt was treated as cleaned: %v", err)
	}
	if err := provider.PurgeLearningReceipt(context.Background(), "missing"); !errors.Is(err, ErrReceiptNotFound) {
		t.Fatalf("missing receipt was treated as cleaned: %v", err)
	}
	if err := provider.PurgeLearningReceipt(context.Background(), "forbidden"); err == nil {
		t.Fatal("permission rejection was treated as cleaned")
	}
	if err := provider.PurgeLearningReceipt(context.Background(), strings.Repeat("x", 129)); err == nil {
		t.Fatal("invalid operation key was sent to the gateway")
	}
	if calls.Load() != 4 {
		t.Fatalf("cleanup invoked an unexpected request: %d", calls.Load())
	}
}
