# Rin

Rin (琳) is the open chat core used by Rinspace. This Go module builds Rin's chat messages and calls an OpenAI-compatible model gateway through an injected provider. It returns answer text, model request ID, and token usage.

The host supplies authentication, conversation history, page context, memory, retrieval, storage, wallet billing, and rate limits. This module contains no Rinspace user data or runtime secrets. The included `OpenAIProvider` can call a separately deployed new-api gateway; new-api is an independent AGPL-3.0 service and is not copied or linked into this module.

## Separate service

`cmd/rin-sidecar` runs this AGPL source as an independent HTTP process. The host sends a chat snapshot to `POST /v1/prepare`, persists the returned completion request before charging or calling the model, then sends the frozen request and its operation ID to `POST /v1/complete`. It uses `GET /v1/operations/{id}` to recover the original receipt and `POST /v1/operations/{id}/abort` to atomically terminate an operation that was never claimed. Recovery does not send a second model request.

Set `RIN_SIDECAR_TOKEN` to a random secret of at least 32 characters, `RIN_GATEWAY_ENDPOINT` to the internal new-api `/v1/chat/completions` endpoint, and `RIN_GATEWAY_API_KEY` to a dedicated gateway token. Optional settings are `RIN_SIDECAR_ADDR` (default `:8098`) and `RIN_MAX_TOKENS` (default `1200`). All `/v1` routes require `Authorization: Bearer <RIN_SIDECAR_TOKEN>`. Bind this service only to a private network; never publish its port or tokens to a browser. `GET /health` reports process health.

The container is built from [Dockerfile](Dockerfile). Source for every published image remains available in this AGPL repository. The host must pin a reviewed image by digest and apply its own account and wallet checks before any `/v1/complete` call.

## Go library

Other AGPL consumers can import the package directly:

```go
provider := rin.OpenAIProvider{
    Endpoint: "http://internal-gateway:3000/v1/chat/completions",
    APIKey:   gatewayToken,
}
client := rin.Client{Provider: provider, Model: rin.DefaultModel}
reply, err := client.Chat(ctx, rin.ChatInput{
    OperationID: operationID,
    AuthorName:  displayName,
    History:     []rin.Message{{Role: "user", Content: message}},
})
```

`Chat` does not retry a model request. A host that charges users must first reserve funds, then persist the returned usage and settle or recover the original operation ID. Never send the gateway token to a browser.

`OperationID` is sent as `X-Rinspace-Operation-Key`; the gateway must enforce one call per key and expose the original receipt for recovery.

After a timeout, call `OpenAIProvider.ReadReceipt(ctx, operationID)`. It returns the original answer and usage for a completed operation, or a durable `failed`, `unknown`, or `expired` status. A response without the gateway's operation status header is an error; the host must keep that operation unresolved instead of issuing a second model call.

License: AGPL-3.0-only, matching Rinspace Web. Rinspace pins the reviewed sidecar image by OCI digest; its private account and wallet service does not import this Go package. Each published image and corresponding source are listed in this repository's releases.
