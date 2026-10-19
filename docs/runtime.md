# Runtime reservations

`mgw serve --runtime FILE` reads `max_reservation_tokens` from the runtime JSON.
The default is **2000** when the field is absent. An explicit positive value
overrides the default. Zero and negative values are invalid in the runtime file.
`mgw authority` also writes a 2000-token ceiling.

One gateway-mode run measured completions of 70–807 output tokens. The old
8000-token ceiling was about ten times the largest completion. The new ceiling
is about 2.5 times that completion. It keeps roughly 30 aborted streams inside
a 100k run budget instead of 12, allowing for other usage.

The ceiling bounds each reservation, not the measured charge at settlement.
An aborted stream with unknown usage keeps its reservation as an uncertain
charge. Choose a larger ceiling when the workload needs it.

## Provider base URLs

A provider `base_url` can carry an optional API path prefix. The gateway
preserves the prefix verbatim and appends the request path without URL resolution
or path normalization. For example, `https://openrouter.ai/api` plus
`/v1/chat/completions` produces `https://openrouter.ai/api/v1/chat/completions`.
A host-root base such as `https://api.openai.com` also works.

Use HTTPS. The only HTTP exception is the explicit test base
`http://127.0.0.1[:port]`, which also accepts a path prefix. The base URL must not
contain userinfo, a query string, a fragment, a trailing slash, a `..` path
segment, or a double slash in its path. Validation also checks decoded path
segments, so percent encoding cannot bypass these path restrictions.
