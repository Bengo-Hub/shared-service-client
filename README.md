# Shared Service Client

Standardized HTTP client library for service-to-service communication across BengoBox microservices.

**Repository:** `github.com/Bengo-Hub/shared-service-client`

## Installation

### Production (Recommended)

Import as a Go module in your service:

```go
require (
    github.com/Bengo-Hub/shared-service-client v0.1.0
)
```

Then:
```bash
go mod tidy
```

### Local Development (Go Workspace)

When developing locally, clone all repositories into a parent directory (e.g., `BengoBox/`) and use `go.work`:

```bash
cd BengoBox/
go work init \
  ./subscription-service \
  ./logistics-service \
  ./shared/service-client
```

See `SETUP.md` for detailed setup instructions.

## Features

- ✅ **Circuit Breaker** - Prevents cascading failures using gobreaker
- ✅ **Retry with Exponential Backoff** - Automatic retries for transient failures
- ✅ **Distributed Tracing** - OpenTelemetry integration for request tracing
- ✅ **Structured Logging** - Request/response logging with Zap
- ✅ **Timeout Configuration** - Configurable timeouts per service
- ✅ **Service Discovery Ready** - Works with Kubernetes DNS service names

## Usage

```go
import serviceclient "github.com/Bengo-Hub/shared-service-client"

// Create client with defaults
cfg := serviceclient.DefaultConfig(
    "http://auth-api.auth.svc.cluster.local:4101",
    "auth-service",
    logger,
)
client := serviceclient.New(cfg)

// GET request
resp, err := client.Get(ctx, "/api/v1/users/"+userID, nil)
if err != nil {
    return err
}

var user map[string]interface{}
if err := resp.DecodeJSON(&user); err != nil {
    return err
}

// POST request
body := map[string]interface{}{
    "email": "user@example.com",
    "tenant_slug": "acme",
}
resp, err := client.Post(ctx, "/api/v1/users", body, nil)
```

## Configuration

```go
cfg := &serviceclient.Config{
    BaseURL:     "http://treasury-api.treasury.svc.cluster.local:4000",
    ServiceName: "treasury-service",
    Timeout:     15 * time.Second,
    Logger:      logger,
    
    // Circuit breaker settings
    MaxRequests: 3,
    Interval:    60 * time.Second,
    TimeoutCB:   30 * time.Second,
    
    // Retry settings
    InitialInterval:     100 * time.Millisecond,
    MaxInterval:         5 * time.Second,
    MaxElapsedTime:      30 * time.Second,
    Multiplier:          2.0,
    RandomizationFactor: 0.5,
}
```

## Circuit Breaker

The circuit breaker prevents requests when a service is failing:
- **Closed**: Normal operation, requests pass through
- **Open**: Service is failing, requests fail fast
- **Half-Open**: Testing if service recovered, limited requests allowed

Defaults: Opens after 5 consecutive failures, closes after 30 seconds.

## Retry Logic

Automatic retries with exponential backoff:
- Retries on network errors and HTTP 5xx/429 status codes
- Uses exponential backoff (100ms → 5s max)
- Maximum retry time: 30 seconds

## Distributed Tracing

All requests automatically create OpenTelemetry spans:
- Span name: `{METHOD} {path}`
- Attributes: `http.method`, `http.url`, `service.name`, `http.status_code`
- Errors are recorded in spans

## Integration

Replace direct `http.Client` usage with `serviceclient.Client`:

```go
// Before
resp, err := http.Get("http://auth-service/api/v1/users/123")

// After
client := serviceclient.New(serviceclient.DefaultConfig(...))
resp, err := client.Get(ctx, "/api/v1/users/123", nil)
```

