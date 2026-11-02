# Request Limits

The Platform API enforces limits on inbound request sizes to prevent memory
exhaustion from oversized payloads.

## Request body size

All API endpoints share a global maximum request body size enforced by the
`LimitRequestBody` middleware. Enforcement has two paths depending on whether
the client declares a `Content-Length` header:

- **With `Content-Length`**: if the declared size exceeds the limit, the
  middleware returns `413 Request Entity Too Large` before any body bytes are
  read and before the handler is invoked.
- **Without `Content-Length`** (chunked or streamed): the body is wrapped with
  `http.MaxBytesReader`. When a handler reads beyond the limit it receives a
  `*http.MaxBytesError`. The middleware does not guarantee a 413 in this path —
  the handler's own error handling determines the response status code.

| Setting                  | Default    | Description                                     |
| ------------------------ | ---------- | ----------------------------------------------- |
| `MAX_REQUEST_BODY_BYTES` | `10485760` | Maximum request body in bytes (default: 10 MiB) |

The default of 10 MiB matches the AWS API Gateway maximum payload size, so
legitimate requests passing through the gateway are never rejected by this
limit. If an endpoint requires a larger body, set `MAX_REQUEST_BODY_BYTES`
at deploy time.

### Example

```bash
# Override the limit to 4 MiB
MAX_REQUEST_BODY_BYTES=4194304 ./platform-api serve
```

## Request header size

The API server caps total request header size at 16 KiB (`MaxHeaderBytes` on
the `http.Server`). This is sufficient for SigV4 `Authorization` headers
(~300 bytes) and session tokens (~400 bytes) with a large safety margin.

| Setting                   | Default | Description                                      |
| ------------------------- | ------- | ------------------------------------------------ |
| `MAX_REQUEST_HEADER_BYTES` | `16384` | Target maximum request header block in bytes (16 KiB). Go adds 4096 bytes of internal buffer slop, so the actual accepted size is up to `MAX_REQUEST_HEADER_BYTES + 4096`. |

## Server timeouts

The API server enforces connection timeouts on all three HTTP servers (API,
health, metrics). The health and metrics server timeouts are fixed; only the
API server values are configurable.

| Setting                       | Default | Description                                                  |
| ----------------------------- | ------- | ------------------------------------------------------------ |
| `READ_HEADER_TIMEOUT_SECONDS` | `10`    | Seconds to wait for request headers before closing (Slowloris protection) |
| `READ_TIMEOUT_SECONDS`        | `30`    | Seconds to read the complete request including body          |
| `WRITE_TIMEOUT_SECONDS`       | `30`    | Seconds from when request headers are read until the response is fully written; includes handler execution time |
| `IDLE_TIMEOUT_SECONDS`        | `60`    | Seconds before an idle keep-alive connection is closed       |

All timeout values must be positive integers. Zero or absent values fall back
to the defaults shown above.

`READ_HEADER_TIMEOUT_SECONDS` is capped at the effective `READ_TIMEOUT_SECONDS`
(including its default). If the configured header timeout would exceed the total
read timeout, the smaller value is used for the header deadline.
