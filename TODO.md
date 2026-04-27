# TODO

Items surfaced during the 2026-04-27 review that are deliberately deferred. Pick up when prioritized.

## A1 Part 2 — wildcard users: share limit state across requests

**Issue.** Every request authenticated as a wildcard user calls `deepCopy(template)` (scope.go:675-689) via `generateWildcardedUserInformation` (proxy.go:888-908) and gets a *fresh* `clusterUser`. That fresh struct has its own zero-valued `queryCounter`, its own empty `queueCh`, and (post-PR #7) its own fresh `rate.Limiter`. So per-cluster-user limits that depend on accumulated state — `max_concurrent_queries`, `max_queue_size`, `req_per_min`, and the packet-size *rate* (not the per-request burst, which #7 already enforces) — never actually constrain anything across requests for a wildcard identity.

Documented upstream as a known limitation in `docs/src/content/docs/configuration/users.md:25`. For this fork, it is a real correctness gap.

**Sketch.** Cache cloned cluster users on the `cluster` struct, keyed by `(template *clusterUser, resolvedName string)`. On wildcard auth, look up; on miss, `deepCopy` and store. Subsequent requests for the same resolved identity reuse the same `*clusterUser` and therefore share `queryCounter`, `queueCh`, and the rate limiter. ~70-100 LoC including tests.

**Risks to handle.**
- Unbounded growth if wildcard usernames are unstable (e.g. session-derived). Cap with an LRU or document the assumption that wildcard identities are stable.
- Cache resets on config reload because `applyConfig` swaps `clusters`. Probably fine; reloads are rare.
- The cached entry's `name`/`password` are now pinned. Today `deepCopy` mutates them at proxy.go:897-898 *after* return — that mutation must happen before cache insertion.
- Two templates that both match the same resolved name (e.g. `*-UK` and `*` both matching `john-UK`): keep the key as `(template, resolvedName)` so the cache stays consistent with the first-match resolution in `findWildcardedUserInformation`.

## F — migrate off `http.CloseNotifier`

**Issue.** `listenToCloseNotify` (proxy.go:355-376) and `statResponseWriter` (io.go:24, io.go:118-119) use `http.CloseNotifier`, deprecated since Go 1.11 in favor of `Request.Context()`. Module is `go 1.24`. Not broken — `CloseNotifier` still works — but it is the only legacy reason for the per-request goroutine in `listenToCloseNotify` and the `BUG:` panic invariant at proxy.go:363.

**Sketch.** Replace `listenToCloseNotify` with `req.Context()` directly (cancels on client disconnect since Go 1.7). Drop the `CloseNotifier` implementation from `statResponseWriter` and any tests that wrap it (`testCloseNotifier` in proxy_test.go). Removes one goroutine per request.

**Risks to handle.**
- Audit every middleware/wrapper that today produces a `ResponseWriterWithCode`; nothing should still depend on the deprecated interface after the migration.
- Make sure `req.Context()` cancellation actually propagates through the existing `executeWithRetry` and timeout-handling paths the way `CloseNotifier` did.
