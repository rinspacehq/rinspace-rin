FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY rin.go http_provider.go ./
COPY cmd/rin-sidecar ./cmd/rin-sidecar
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /rin-sidecar ./cmd/rin-sidecar

FROM alpine:3.22
RUN addgroup -S rin && adduser -S -G rin rin
COPY --from=build --chown=rin:rin /rin-sidecar /usr/local/bin/rin-sidecar
USER rin
EXPOSE 8098
ENTRYPOINT ["/usr/local/bin/rin-sidecar"]
