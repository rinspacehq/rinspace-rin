package rin

import (
	"context"
	"errors"
	"net/http"
)

var ErrReceiptPending = errors.New("rin receipt is not terminal")

// PurgeLearningReceipt removes only the original gateway's terminal learning
// response body. The product must first confirm settlement of the original
// operation. It does not abort, reclaim, or invoke any model request.
func (p OpenAIProvider) PurgeLearningReceipt(ctx context.Context, operationID string) error {
	if !validOperationID(operationID) {
		return errors.New("rin operation ID is invalid")
	}
	endpoint, err := p.agentEndpoint()
	if err != nil {
		return err
	}
	endpoint.Path = "/v1/rin/operations/" + operationID + "/content"
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+p.APIKey)
	request.Header.Set("X-Rinspace-Budget-Profile", ReaderLearningBudgetV1)
	response, err := p.agentHTTPClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return ErrReceiptPending
	case http.StatusNotFound:
		return ErrReceiptNotFound
	default:
		return errors.New("rin learning receipt content cleanup is unavailable")
	}
}
