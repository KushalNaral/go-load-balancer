# Go Load Balancer — Architecture & Design

## What It Is

A reverse-proxy load balancer in Go. It accepts HTTP requests, picks a healthy
backend from a pool, and forwards the request using `httputil.ReverseProxy`.
It detects failing backends both **actively** (periodic health probes) and
**passively** (observing real request failures), and routes around them.

This document is the *theory*. Code lives elsewhere.

---

## Mental Model: Three Loops, One Data Structure

All the behavior of the LB can be described as three concurrent loops that
share one data structure.

```
            +------------------------------------------+
            |                ServerPool                |
            |   immutable []*Backend + RR counter      |
            +------------------------------------------+
                ^              ^              ^
       reads    |     writes   |     reads    |
       status   |     status   |     config   |
                |              |              |
    +-----------+--+   +-------+------+   +---+------------+
    | Serving loop |   | Health loop  |   | Lifecycle loop |
    | (hot path)   |   | (background) |   | (signals)      |
    +--------------+   +--------------+   +----------------+
```

**Invariant:** the serving loop *reads* backend status; the health loop
*writes* it. Single-writer means no lock contention on the hot path and easy
reasoning about races.

### Loop 1 — Serving (hot path)
1. `net/http` accepts a connection.
2. Handler calls `pool.NextHealthy()` → returns a `*Backend` or `nil`.
3. `nil` → respond `503 Service Unavailable`.
4. Otherwise: `backend.ReverseProxy.ServeHTTP(w, r)`.
5. On transport error, `ReverseProxy.ErrorHandler` fires — this is the
   *passive* health signal. Mark the backend suspect; optionally retry on
   another peer (subject to the retry rules below).

The handler never blocks on health checks; it only reads status.

### Loop 2 — Active health checker (background)
- A `time.Ticker` fires every `HealthCheckInterval`.
- For each backend, perform an **HTTP GET** to a `/health` path with a short
  timeout. (Prefer HTTP over raw TCP dial: TCP tells you the port is open,
  not that the app is sane.)
- Update `Backend.Status`. Log only on *transitions* (healthy↔unhealthy),
  not on every tick.
- Governed by a `context.Context` so it stops cleanly on shutdown.

### Loop 3 — Lifecycle (signals)
- `signal.NotifyContext` for `SIGINT`/`SIGTERM`.
- On signal:
  1. Stop accepting new connections (`http.Server.Shutdown`).
  2. Cancel the health-checker context.
  3. Wait for in-flight requests up to a deadline, then exit.

---

## Core Types

### `Backend`
Represents one upstream server.

```
Backend
  URL          *url.URL
  Status       atomic.Int32   // typed: StatusHealthy | StatusUnhealthy
  ReverseProxy *httputil.ReverseProxy
```

- `Status` is a typed enum, **not a bool**. Future states (draining,
  probation) slot in without a rewrite.
- Reads via `atomic.Load`; writes only from the health loop.

### `ServerPool`
Owns backends and selection logic.

```
ServerPool
  backends []*Backend   // immutable after construction
  counter  atomic.Uint64 // round-robin cursor
  NextHealthy() *Backend
```

- The slice is set once at init and never mutated — no locks needed for
  iteration.
- `NextHealthy`: atomically increment the counter, then linearly scan up to
  `len(backends)` entries starting at `counter % N`, returning the first
  healthy one. Returns `nil` if all are unhealthy.
- **Known quirk:** under concurrent load, two requests can pick the same
  backend, so distribution isn't strictly round-robin. Acceptable for v1.

### `HealthChecker`
Owns the ticker loop and probe logic. Takes a `context.Context` and a pool.

### `Config`
Parsed once at startup, passed around explicitly — never global, never
mutated after `New`.

```
Config
  Port                int
  Backends            []string
  HealthCheckInterval time.Duration
  ProbeTimeout        time.Duration
  ReadHeaderTimeout   time.Duration
  WriteTimeout        time.Duration
  IdleTimeout         time.Duration
  ShutdownTimeout     time.Duration
```

---

## Status Is Eventually Consistent

There is **always** a window where a dead backend still looks healthy, or
vice versa. Design for this instead of trying to eliminate it.

- Hot path **tolerates** stale status: passive detection + safe retry.
- Active checker **corrects** stale status over time.

Chasing real-time accuracy here leads to locks on the hot path and
complexity that doesn't pay off.

---

## Retry Policy (Important — get this right up front)

When a request fails through `ReverseProxy`, we may want to retry on another
backend. The rule:

> **Retry only if (a) the method is idempotent (GET/HEAD/PUT/DELETE/OPTIONS)
> AND (b) no response bytes have been written to the client yet.**

Why: once bytes of the request body have streamed to backend A, we can't
replay them to backend B — the body is gone. Once bytes of the response
have streamed to the client, we can't take them back. Non-idempotent
methods (POST/PATCH) must never be retried blindly because the original
might have succeeded server-side before the transport error.

On a non-retryable failure: return `502 Bad Gateway` cleanly, do not hang.

---

## Active vs Passive Health Checks

| | Active (health loop) | Passive (error handler) |
|---|---|---|
| When | On a timer | On real request failure |
| Cost | Constant background traffic | Free — piggybacks on real traffic |
| Latency | Up to one interval behind | Immediate |
| Confidence | Low (synthetic request) | High (real failure observed) |

Both are needed. Passive is the primary signal; active is the recovery
signal (it's the only way a backend marked dead gets reinstated).

---

## Error Handling Matrix

| Scenario | Behavior |
|---|---|
| All backends unhealthy | `503 Service Unavailable` |
| Selected backend fails, request retryable | Retry on next healthy peer, mark original unhealthy |
| Selected backend fails, request not retryable | `502 Bad Gateway`, mark unhealthy |
| Client disconnects mid-request | `r.Context()` cancels upstream call (ReverseProxy handles this) |
| LB receives SIGTERM | Stop accepting, drain in-flight up to `ShutdownTimeout` |

---

## Security & Robustness Baseline

Non-negotiable from day one:

- `http.Server.ReadHeaderTimeout` — prevents slowloris.
- `ReadTimeout`, `WriteTimeout`, `IdleTimeout` — bound every connection.
- Health probe has its own short timeout; doesn't use the server's.
- No global state; config passed explicitly.
- Structured logging via `slog`; request-scoped fields.

---

## Implementation Steps & Done-Criteria

Each step has an **observable** signal for "done" — not a vibe.

### Step 1 — Scaffold + `Backend`
- `go mod init`, minimal `main.go` that compiles.
- `Backend` struct with `Status` as a typed enum (iota constants), atomic
  load/store, and a constructor that builds the `ReverseProxy` from a URL.
- **Done when:** `go build` works and a unit test constructs a Backend,
  flips its status, and reads it back.

### Step 2 — `ServerPool` + round-robin (no HTTP yet)
- `NextHealthy()` implemented with atomic counter + linear scan.
- **Done when:** table test with 3 backends, 9 calls asserts order
  `[0,1,2,0,1,2,0,1,2]`; same test with backend[1] unhealthy asserts
  `[0,2,0,2,...]`; all-unhealthy returns `nil`.

### Step 3 — Reverse proxy handler with hardened `http.Server`
- Handler: `pool.NextHealthy()` → `nil` ⇒ 503, else delegate to
  `backend.ReverseProxy`.
- `http.Server` with all four timeouts set.
- **Done when:** `curl localhost:PORT` returns a response from a real
  backend (spin up two `python -m http.server`), logs show which backend
  served it, distribution rotates across requests.

### Step 4 — Active health checker
- `time.Ticker`-driven loop, context-cancelable, HTTP GET `/health`.
- Log only on transitions.
- **Done when:** killing a backend process marks it unhealthy within one
  interval; restarting it marks it healthy within one interval;
  transitions logged exactly once each.

### Step 5 — Passive health signal + retry
- `ReverseProxy.ErrorHandler` marks backend unhealthy and retries per the
  retry policy above.
- **Done when:** killing a backend *mid-request* yields: successful
  failover for idempotent requests, a clean 502 for non-idempotent
  requests, never a hang.

### Step 6 — Graceful shutdown
- `signal.NotifyContext`, `http.Server.Shutdown(ctx)` with
  `ShutdownTimeout`, health loop exits on ctx cancel.
- **Done when:** sending SIGTERM while a slow request is in-flight lets
  that request complete before the process exits; no new connections
  accepted during drain.

### Step 7 — End-to-end + race gate
- Run 3 backends, fire load (`hey` / `wrk`), kill one mid-flight.
- **Done when:** zero client errors on GETs, `go test -race ./...` clean,
  distribution across survivors visible in logs.

### Stretch (after v1 ships)
- Config file (YAML/JSON).
- `/metrics` endpoint (Prometheus).
- Weighted round-robin / least-connections.
- TLS termination.
- Sticky sessions (cookie or IP hash).

---

## Project Layout

```
go-load-balancer/
  main.go       -- wiring: config → pool → health checker → server
  config.go     -- flag parsing + validation
  backend.go    -- Backend type, Status enum, constructor
  pool.go       -- ServerPool, NextHealthy
  health.go     -- HealthChecker loop + probe
  proxy.go      -- ErrorHandler, retry policy
  *_test.go
  go.mod
```

---

## Key stdlib packages

| Package | Why |
|---|---|
| `net/http` | LB server |
| `net/http/httputil` | `ReverseProxy` |
| `net/url` | Parse backend URLs |
| `sync/atomic` | Status + RR counter |
| `context` | Cancellation for health loop + shutdown |
| `time` | Ticker, timeouts |
| `log/slog` | Structured logging |
| `flag` | Config parsing |
| `os/signal` | SIGTERM handling |
