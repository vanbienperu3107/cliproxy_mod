# DeepSeek web plugin

A CLIProxyAPI v8 provider plugin that serves DeepSeek models through the
`chat.deepseek.com` web endpoints using a browser `userToken`. It implements the
auth provider, model provider, and chat-completions executor entirely inside the
plugin; the host core is not modified.

## Requirements

- CLIProxyAPI v8 with plugin ABI schema 6 (`sdk/pluginabi.SchemaVersion`).
- A C toolchain for `-buildmode=c-shared` (or Zig: `CC="zig cc"`).
- A `userToken` copied from `localStorage` on `chat.deepseek.com`.

## Build

```sh
cd examples/plugin/deepseek-web/go
CGO_ENABLED=1 go build -buildmode=c-shared -o bin/deepseek-web-go.so .
```

Copy `bin/deepseek-web-go.so` into the host's plugin directory. The plugin ID is
the file name without `.so`: `deepseek-web-go`.

## Host configuration

```yaml
plugins:
  enabled: true
  dir: "/absolute/path/to/plugins"
  configs:
    deepseek-web-go:
      enabled: true
```

The per-plugin `enabled: true` entry is required. A discovered plugin defaults to
disabled.

## Credential

```json
{
  "type": "deepseek-web",
  "userToken": "<token from chat.deepseek.com localStorage>",
  "prefix": "deepseek-web",
  "persistSession": true
}
```

Place it in the host auth directory (for example `auths/deepseek.json`).

| Field | Meaning |
| --- | --- |
| `userToken` | Required. Browser token; also accepts `user_token`, `apiKey`, `token`, `value`. |
| `baseUrl` | Optional. Completion URL; defaults to `https://chat.deepseek.com/api/v0/chat/completion`. Sibling endpoints are derived from its origin. |
| `cookie` | Optional. Extra `Cookie` header for the upstream. |
| `prefix` | Optional. Host-level model prefix. When set, the host also registers `deepseek-web/<model>`. |
| `persistSession` | Default `true`. Pins one upstream chat session per client conversation. |
| `historyWindow` | Default 20. Messages kept when rendering the prompt. |
| `maxConcurrency` | Default 2. Per-account in-flight limit. |
| `jitterMs` | Default 400. Random pause before each turn. |

## Models

`deepseek-v4-pro`, `deepseek-v4-pro-think`, `deepseek-v4-pro-search`,
`deepseek-v4-pro-think-search`, `deepseek-v4-flash`,
`deepseek-v4-flash-think`, `deepseek-v4-flash-search`,
`deepseek-v4-flash-think-search`, `deepseek-chat`, `deepseek-reasoner`.

Model IDs are registered bare so the host can apply its own auth prefix.

## Behaviour

- One turn is four upstream calls: `users/current` (token exchange, cached),
  `chat_session/create`, `chat/create_pow_challenge` + `DeepSeekHashV1` solve,
  `chat/completion` (SSE), then `chat_session/delete` unless the session is
  pinned.
- The web API accepts a single prompt string. Messages and tool declarations are
  rendered into that prompt; tool calls are parsed back into OpenAI
  `tool_calls` (JSON or DSML). Requests with `tools` buffer the whole answer
  before emitting calls so a truncated JSON call is never reported as valid.
- Streaming emits bare OpenAI chunks. The host adds `data:` framing and the
  final `[DONE]`.
- Session affinity: send `X-Session-Id` (or `session_id` metadata). With
  `persistSession`, a follow-up whose prompt extends the previous one reuses the
  upstream session and sends only the new tail plus `parent_message_id`.
- Account refusals delivered as HTTP 200 are mapped to HTTP status: muted /
  rate limited -> 429, banned / IP restricted -> 403, other refusals -> 502.
  An opening refusal event is read before the stream is committed so it still
  reaches the client as a status.
- `userToken` is not renewable without a browser; re-paste it when it expires.

## Test

```sh
CGO_ENABLED=0 go test ./... -count=3 -shuffle=on
CGO_ENABLED=0 go vet ./...
```

The unit suite covers PoW vectors, prompt/tool parsing, stream accumulation,
citations, error mapping, and executor streaming against a fake upstream. Live
verification against a real DeepSeek account is not included.

## Caveats

Using the web endpoints is unofficial and may violate DeepSeek's terms. Accounts
can be muted or banned. Prefer the official API for production traffic.
