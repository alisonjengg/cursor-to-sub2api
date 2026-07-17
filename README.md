# cursor-to-sub2api

A small reverse proxy for routing Cursor's OpenAI-compatible requests to a Sub2API-compatible upstream. It rewrites two request shapes before forwarding:

- `POST /v1/chat/completions` — removes the top-level `user` field.
- `POST /v1/responses` — truncates any `call_id` longer than 64 characters (the limit many OpenAI-compatible upstreams enforce). Truncation is deterministic, so paired `function_call` / `function_call_output` ids stay equal.

Other request paths and response streams are proxied unchanged.

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `UPSTREAM_URL` | Yes | - | Absolute HTTP(S) URL of the upstream API |
| `LISTEN_ADDR` | No | `:8080` | Address the proxy listens on inside the container |
| `MAX_BODY_BYTES` | No | `67108864` | Maximum inspected request size (64 MiB) |
| `LOG_REQUEST_BODY` | No | `false` | Log full request bodies; may expose sensitive content |

The service exposes `GET /healthz` locally and returns `200 OK`. This path is not forwarded upstream.

## Run with Docker

```bash
docker build -t cursor-to-sub2api .
docker run --rm -p 18081:8080 \
  -e UPSTREAM_URL=https://your-sub2api.example.com \
  cursor-to-sub2api
```

Point Cursor's OpenAI base URL at `http://localhost:18081` (or the HTTPS domain used for your deployment).

For local Docker Compose usage:

```bash
UPSTREAM_URL=https://your-sub2api.example.com docker compose up --build
```

## Deploy with Coolify

1. Push this repository to GitHub and create a new **Application** in Coolify from that repository.
2. Select **Dockerfile** as the build pack. The Dockerfile is at `/Dockerfile` and the exposed port is `8080`.
3. Add `UPSTREAM_URL` as a runtime environment variable. Include any path prefix required by the upstream, for example `https://api.example.com/openai`.
4. Add a domain to the application and set its container port to `8080`.
5. Set the health-check path to `/healthz`, port `8080`, and expected status code `200`.
6. Deploy, then configure Cursor to use the application's HTTPS domain as its OpenAI base URL.

Do not enable `LOG_REQUEST_BODY` in a shared or public deployment unless full chat payloads are intentionally being retained in Coolify logs. This proxy does not provide authentication; use a private network, upstream API authentication, or Coolify's access controls as appropriate.

## Development

```bash
go test ./...
UPSTREAM_URL=http://127.0.0.1:8080 go run .
```

## License

MIT
