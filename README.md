<p align="center">
  <img src="assets/icon.svg" width="88" height="88" alt="OpenCode Go Gateway icon">
</p>

<h1 align="center">OpenCode Go Gateway</h1>

<p align="center">Free OpenCode chat models behind an OpenAI-compatible API. One Go binary.</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/github/go-mod/go-version/shyim/opencode-go-gateway" alt="Go version"></a>
  <a href="#getting-started">Getting started</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#api-reference">API reference</a> ·
  <a href="#development">Development</a>
</p>

Use OpenCode's free chat models from apps that support an OpenAI Chat Completions base URL. The gateway discovers eligible models from the OpenCode v2 catalog and forwards inference requests directly to OpenCode. It runs without the OpenCode CLI, Bun, or local model weights.

- **Streaming and JSON responses** — relay server-sent events as they arrive or receive a complete JSON response.
- **Dynamic model discovery** — list text chat models whose catalog prices are zero, with a five-minute cache.
- **Optional authentication** — protect the gateway with a bearer token and configure upstream credentials separately.
- **Session routing** — preserve conversation affinity, child sessions, and project identifiers.
- **CLI transport compatibility** — use OpenCode-style request headers and a Bun-compatible TLS profile with certificate verification enabled.

> **Availability depends on OpenCode.** The public provider defaults to the built-in `public` credential. Catalog listings do not guarantee access, availability, or billing behavior. OpenCode can change its free-tier checks, model offerings, and rate limits at any time. This is an independent project.

## Getting started

Requires **Go 1.24 or newer** to build. The compiled binary needs no Go installation.

```sh
git clone https://github.com/shyim/opencode-go-gateway.git
cd opencode-go-gateway
go build -o opencode-go-gateway .
./opencode-go-gateway
```

The gateway listens on `http://127.0.0.1:8080` by default. For local development, you can also run `go run .`.

### Run in a container

Pull the published image for Linux AMD64 or ARM64:

```sh
docker run --rm -p 127.0.0.1:8080:8080 ghcr.io/shyim/opencode-go-gateway:latest
```

The [container workflow](.github/workflows/container.yml) uses [Docker's GitHub Builder](https://github.com/docker/github-builder) to build both architectures, generate an SBOM, and sign provenance attestations. Pushes to `main` publish `latest`, `main`, and `sha-<commit>` tags. Version tags such as `v1.2.3` publish `1.2.3`, `1.2`, and a commit tag. Pull requests build without publishing. Publishing uses the built-in `GITHUB_TOKEN`; no registry secrets are required.

To build locally:

```sh
docker build -f Containerfile -t opencode-go-gateway .
docker run --rm -p 127.0.0.1:8080:8080 opencode-go-gateway
```

The multi-stage build produces a static binary and runs it as a non-root user in a [distroless](https://github.com/GoogleContainerTools/distroless) image. The image listens on `0.0.0.0:8080` inside the container; the command above publishes it on the host's loopback address. Pass configuration with `-e` and append gateway flags after the image name:

```sh
docker run --rm -p 127.0.0.1:8080:8080 \
  -e GATEWAY_API_KEY -e OPENCODE_API_KEY \
  opencode-go-gateway -timeout 5m
```

The runtime image includes CA certificates for outbound HTTPS and has no shell or package manager.

### Discover models

```sh
curl -sS http://127.0.0.1:8080/v1/models
```

Copy a model ID from the response into the following requests. IDs use the format `opencode/<model>` or `opencode-go/<model>`. A bare model ID also works when it is unique across providers.

### Send a message

Set `MODEL` to an ID returned by `/v1/models`:

```sh
MODEL='opencode/your-model-id'

curl -sS http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello.\"}]}"
```

### Stream a response

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Explain Go interfaces.\"}],\"stream\":true}"
```

### Connect an app

| Client setting | Value |
| --- | --- |
| Base URL | `http://127.0.0.1:8080/v1` |
| Model | An ID returned by `/v1/models` |
| API key | Your `GATEWAY_API_KEY`, or any placeholder if your app requires a key and gateway authentication is disabled |

The gateway uses its configured upstream credential. It never forwards the caller's bearer token as the OpenCode API key.

## Configuration

Command-line flags override their corresponding environment variables. There is no configuration file.

| Environment variable | Flag | Default | Purpose |
| --- | --- | --- | --- |
| `GATEWAY_LISTEN` | `-listen` | `127.0.0.1:8080` | HTTP listen address |
| `GATEWAY_API_KEY` | — | Unset | Require a bearer token on `/v1/*` routes |
| `OPENCODE_API_KEY` | — | `public` | Upstream OpenCode credential |
| `MODELS_CATALOG_URL` | `-catalog-url` | `https://models.opencode.ai/experimental/v2.0/api.json?type=chat` | Model catalog URL |
| `OPENCODE_INFERENCE_URL` | `-console-url` | `https://opencode.ai/inference/openai/v1` | OpenCode inference base URL |
| `OPENCODE_GO_INFERENCE_URL` | `-go-url` | `https://opencode.ai/inference/go/openai/v1` | OpenCode Go inference base URL |
| `OPENCODE_CLIENT_VERSION` | — | `2.2.5` | Version in the upstream user agent |
| `OPENCODE_CLIENT` | — | `cli` | Upstream client name |
| `OPENCODE_CLIENT_CHANNEL` | — | `latest` | Channel in the upstream user agent |
| — | `-timeout` | `3m` | Maximum request duration, including streams |

Run `./opencode-go-gateway -help` to see available flags.

### Protect the gateway

```sh
export GATEWAY_API_KEY='replace-with-a-secret'
./opencode-go-gateway -listen 127.0.0.1:8080 -timeout 5m
```

Include the token in client requests:

```sh
curl -sS http://127.0.0.1:8080/v1/models \
  -H "Authorization: Bearer $GATEWAY_API_KEY"
```

The default address accepts local connections only. When exposing the gateway to a network, enable authentication and put it behind a trusted proxy that terminates TLS. `GET /health` remains unauthenticated.

### Enable OpenCode Go

```sh
export OPENCODE_API_KEY='your-opencode-go-api-key'
./opencode-go-gateway
```

A non-public upstream key enables catalog discovery for the `opencode-go` provider. Select an eligible `opencode-go/...` ID from `/v1/models`. The gateway does not import credentials from the OpenCode CLI.

### Use an outbound proxy

HTTPS requests honor `HTTPS_PROXY` and `NO_PROXY`. HTTP and HTTPS CONNECT proxies, including proxy authentication, are supported.

```sh
HTTPS_PROXY=http://127.0.0.1:3128 ./opencode-go-gateway
```

## API reference

| Method | Path | Behavior |
| --- | --- | --- |
| `GET` | `/health` | Returns `{"status":"ok"}`; checks liveness only |
| `GET` | `/v1/models` | Returns an OpenAI-style list of eligible models |
| `POST` | `/v1/chat/completions` | Forwards a chat request; supports `stream: true` |

Chat requests must be JSON objects with a `model` and a nonempty `messages` array. Request bodies are limited to **8 MiB**. The gateway replaces the provider-qualified model ID with the upstream ID and forwards other fields, subject to the compatibility adjustments below. Support for tools, sampling parameters, and other features depends on the selected model.

Upstream HTTP status codes and error bodies are relayed, along with `Retry-After` and `X-Request-Id` when present. Gateway validation and connection errors use an OpenAI-style `error` object. Only the endpoints above are implemented; non-default service tiers are rejected (`auto`, `default`, and `null` are accepted).

### Model selection

Models must be non-deprecated chat models with text input and output, a Chat Completions API, and zero catalog prices for input, output, cache, and overrides. Missing or unsupported price metadata is excluded. Only the `opencode` and `opencode-go` providers are considered.

The catalog is cached for five minutes. If a refresh fails after expiry, the request fails instead of serving an indefinitely stale list. Catalog metadata is a selection filter, not a billing guarantee.

### Sessions and affinity

Reuse a routing identifier across a conversation to preserve upstream affinity:

```sh
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Opencode-Session: conversation-123' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello again.\"}],\"stream\":true}"
```

| Header | Purpose |
| --- | --- |
| `X-Opencode-Session` | Routing affinity; aliases: `X-Session-Affinity`, `X-Session-Id` |
| `X-Opencode-Session-Id` | Current session identifier, independent of routing affinity |
| `X-Opencode-Parent-Session-Id` | Parent session; alias: `X-Parent-Session-Id` |
| `X-Opencode-Project` | Project identifier; defaults to `global` |

When no session identifier is supplied, one is generated per request. Affinity defaults to the parent session, then to the current session, if no routing header is provided. Completion responses include `X-Opencode-Session` and `X-Opencode-Session-Id` so clients can reuse them. Identifiers accept letters, digits, underscores, and hyphens, up to 128 characters. Clients must still send conversation history in `messages`.

## Upstream compatibility

The `opencode` route applies a compatibility envelope based on observed free-tier behavior:

- Adds a system `<env>` block containing `Working directory:` when one is missing.
- Adds a synthetic function tool named `read` and sets `tool_choice: "none"` when no tools are supplied. The gateway does not execute tools.
- Preserves existing tools. If `read` is absent, it is appended only when `tool_choice` is already `"none"`, so an injected tool cannot be selected. Requests with other active tools may still be rejected upstream.
- Requests streaming upstream. For callers requesting JSON, it assembles the events into one completion, including content, reasoning, tool calls, and usage when supplied. Streaming callers receive the upstream events directly.

These adjustments apply to the `opencode` provider, including when an upstream key is configured. The `opencode-go` route forwards requests without this envelope.

Inference requests use an OpenCode-style user agent and session headers. The HTTPS transport uses [uTLS](https://github.com/refraction-networking/utls) with an explicit Bun-compatible ClientHello and HTTP/1.1; certificate and hostname verification remain enabled. These compatibility measures depend on upstream behavior and may need updates when OpenCode changes.

### Troubleshooting

| Symptom | What to check |
| --- | --- |
| `401` from the gateway | Send the configured `GATEWAY_API_KEY` as a bearer token. |
| Upstream `401`, `403`, or `429` | Check upstream credentials, account access, and rate limits. A catalog listing does not prove access. |
| `model_not_found` | Refresh `/v1/models` and use a returned provider-qualified ID. |
| No `opencode-go/...` models | Set a non-public `OPENCODE_API_KEY`; only zero-priced catalog entries are listed. |
| `502` or `504` | Check catalog/inference connectivity, proxy settings, and the request timeout. |
| Events arrive in a batch | Disable response buffering in your reverse proxy and use a streaming client such as `curl -N`. |

## Development

```sh
go build -o opencode-go-gateway .
go test -race ./...
go vet ./...
```

Tests use local catalogs, inference fixtures, TLS servers, and CONNECT proxies without consuming upstream model usage. They cover model filtering, authentication, request validation, streaming, JSON aggregation, session routing, cancellation, certificate rejection, and proxy tunneling.

Issues and pull requests are welcome. Include reproduction steps for bugs, keep changes focused, and run the checks above before submitting a pull request. Avoid including API keys or private request data in reports.
