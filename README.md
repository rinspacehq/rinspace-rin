# Rin

Rin (琳) is the open chat core used by Rinspace. This Go module builds Rin's chat messages and calls an OpenAI-compatible model gateway through an injected provider. It returns answer text, model request ID, and token usage.

The host supplies authentication, conversation history, page context, memory, retrieval, storage, wallet billing, rate limits, and provider credentials. This module contains no Rinspace user data or runtime secrets. The included `OpenAIProvider` can call a separately deployed new-api gateway; new-api is an independent AGPL-3.0 service and is not copied or linked into this module.

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

License: MIT. The Rinspace product integration consumes a reviewed, immutable version of this module.
