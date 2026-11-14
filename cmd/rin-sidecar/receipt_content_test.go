package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rin "github.com/rinspacehq/rinspace-rin"
)

type purgeProvider struct {
	fakeProvider
	ids []string
	err error
}

func (p *purgeProvider) PurgeLearningReceipt(_ context.Context, id string) error {
	p.ids = append(p.ids, id)
	return p.err
}

func TestLearningContentCleanupRequiresOriginalServiceAuthority(t *testing.T) {
	provider := &purgeProvider{}
	service := sidecar{token: strings.Repeat("synthetic-key", 3), provider: provider}
	send := func(token, suffix, body string) int {
		request := httptest.NewRequest(http.MethodDelete, "/v1/learning/operations/original-key/content"+suffix, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		service.handler().ServeHTTP(response, request)
		return response.Code
	}
	if send("wrong", "", "") != http.StatusUnauthorized || send(service.token, "?payer=other", "") != http.StatusBadRequest ||
		send(service.token, "", `{}`) != http.StatusBadRequest || len(provider.ids) != 0 {
		t.Fatal("invalid cleanup reached the provider")
	}
	for _, test := range []struct {
		err    error
		status int
	}{{nil, http.StatusNoContent}, {rin.ErrReceiptPending, http.StatusConflict}, {rin.ErrReceiptNotFound, http.StatusNotFound}} {
		provider.err = test.err
		if got := send(service.token, "", ""); got != test.status {
			t.Fatalf("cleanup status %d, want %d", got, test.status)
		}
	}
	if len(provider.ids) != 3 || provider.ids[0] != "original-key" || provider.agentCalls != 0 || provider.calls != 0 || provider.agentReadID != "" {
		t.Fatal("content cleanup changed model or recovery dispatch")
	}
}
