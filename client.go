package serviceclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	"github.com/cenkalti/backoff/v4"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Client provides a standardized HTTP client for service-to-service communication
// with circuit breaker, retry, distributed tracing, and structured logging.
type Client struct {
	baseURL        string
	httpClient     *http.Client
	circuitBreaker *gobreaker.CircuitBreaker
	retry          retrySettings
	tracer         trace.Tracer
	logger         *zap.Logger
	serviceName    string
}

// maxResponseBytes caps how much of a response body is read into memory.
const maxResponseBytes = 64 << 20

// retrySettings are the exponential backoff parameters. Each call builds its own backoff from them,
// because a backoff carries per-call state and is not safe to share between goroutines.
type retrySettings struct {
	initial, max, elapsed time.Duration
	multiplier, jitter    float64
}

func (r retrySettings) newBackOff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = r.initial
	b.MaxInterval = r.max
	b.MaxElapsedTime = r.elapsed
	b.Multiplier = r.multiplier
	b.RandomizationFactor = r.jitter
	b.Reset()
	return b
}

// statusError marks a response worth retrying (5xx or 429).
type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("retryable status %d: %s", e.status, e.body)
}

// Config configures a service client.
type Config struct {
	BaseURL     string
	ServiceName string // Name of the service being called (for logging/tracing)
	Timeout     time.Duration
	Logger      *zap.Logger

	// Circuit breaker settings
	MaxRequests uint32                      // Max requests in half-open state
	Interval    time.Duration               // Time window for circuit breaker
	TimeoutCB   time.Duration               // Timeout before attempting to close circuit
	ReadyToTrip func(gobreaker.Counts) bool // Custom ready-to-trip function

	// Retry settings
	InitialInterval     time.Duration // Initial retry delay
	MaxInterval         time.Duration // Maximum retry delay
	MaxElapsedTime      time.Duration // Maximum total retry time
	Multiplier          float64       // Backoff multiplier
	RandomizationFactor float64       // Randomization factor (0-1)
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig(baseURL, serviceName string, logger *zap.Logger) *Config {
	return &Config{
		BaseURL:     baseURL,
		ServiceName: serviceName,
		Timeout:     10 * time.Second,
		Logger:      logger,

		// Circuit breaker defaults
		MaxRequests: 3,
		Interval:    60 * time.Second,
		TimeoutCB:   30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures > 5
		},

		// Retry defaults (exponential backoff)
		InitialInterval:     100 * time.Millisecond,
		MaxInterval:         5 * time.Second,
		MaxElapsedTime:      30 * time.Second,
		Multiplier:          2.0,
		RandomizationFactor: 0.5,
	}
}

// New creates a new service client with the provided configuration.
func New(cfg *Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}

	tracer := otel.Tracer("shared-service-client")
	if cfg.ServiceName == "" {
		cfg.ServiceName = "unknown-service"
	}

	// Configure HTTP client
	httpClient := &http.Client{
		Timeout: cfg.Timeout,
	}

	// Configure circuit breaker
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        cfg.ServiceName,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.TimeoutCB,
		ReadyToTrip: cfg.ReadyToTrip,
		OnStateChange: func(name string, from, to gobreaker.State) {
			cfg.Logger.Info("circuit breaker state changed",
				zap.String("service", name),
				zap.String("from", from.String()),
				zap.String("to", to.String()),
			)
		},
	})

	retry := retrySettings{
		initial:    cfg.InitialInterval,
		max:        cfg.MaxInterval,
		elapsed:    cfg.MaxElapsedTime,
		multiplier: cfg.Multiplier,
		jitter:     cfg.RandomizationFactor,
	}

	return &Client{
		baseURL:        cfg.BaseURL,
		httpClient:     httpClient,
		circuitBreaker: cb,
		retry:          retry,
		tracer:         tracer,
		logger:         cfg.Logger.Named("service-client").With(zap.String("service", cfg.ServiceName)),
		serviceName:    cfg.ServiceName,
	}
}

// Response wraps an HTTP response with body.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Get performs a GET request with retry and circuit breaker.
func (c *Client) Get(ctx context.Context, path string, headers map[string]string) (*Response, error) {
	return c.doRequest(ctx, http.MethodGet, path, nil, headers)
}

// Post performs a POST request with retry and circuit breaker.
func (c *Client) Post(ctx context.Context, path string, body interface{}, headers map[string]string) (*Response, error) {
	return c.doRequest(ctx, http.MethodPost, path, body, headers)
}

// Put performs a PUT request with retry and circuit breaker.
func (c *Client) Put(ctx context.Context, path string, body interface{}, headers map[string]string) (*Response, error) {
	return c.doRequest(ctx, http.MethodPut, path, body, headers)
}

// Patch performs a PATCH request with retry and circuit breaker.
func (c *Client) Patch(ctx context.Context, path string, body interface{}, headers map[string]string) (*Response, error) {
	return c.doRequest(ctx, http.MethodPatch, path, body, headers)
}

// Delete performs a DELETE request with retry and circuit breaker.
func (c *Client) Delete(ctx context.Context, path string, headers map[string]string) (*Response, error) {
	return c.doRequest(ctx, http.MethodDelete, path, nil, headers)
}

func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}, headers map[string]string) (*Response, error) {
	// Create span for distributed tracing
	ctx, span := c.tracer.Start(ctx, fmt.Sprintf("%s %s", method, path),
		trace.WithAttributes(
			attribute.String("http.method", method),
			attribute.String("http.url", c.baseURL+path),
			attribute.String("service.name", c.serviceName),
		))
	defer span.End()

	// Marshal once; every attempt gets its own reader over the same bytes.
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
	}

	url := c.baseURL + path
	canRetry := isIdempotent(method, headers)

	var resp *Response
	err := backoff.Retry(func() error {
		req, err := c.newRequest(ctx, method, url, bodyBytes, headers)
		if err != nil {
			return backoff.Permanent(fmt.Errorf("create request: %w", err))
		}
		c.logger.Debug("service request",
			zap.String("method", method),
			zap.String("url", url),
			zap.Any("headers", redactHeaders(req.Header)),
		)

		result, err := c.circuitBreaker.Execute(func() (interface{}, error) {
			httpResp, err := c.httpClient.Do(req)
			if err != nil {
				span.RecordError(err)
				return nil, err
			}
			defer httpResp.Body.Close()

			respBody, readErr := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes+1))
			if readErr != nil {
				return nil, fmt.Errorf("read response: %w", readErr)
			}
			if len(respBody) > maxResponseBytes {
				return nil, errResponseTooLarge
			}

			if httpResp.StatusCode >= 500 || httpResp.StatusCode == http.StatusTooManyRequests {
				return nil, &statusError{status: httpResp.StatusCode, body: truncate(string(respBody), 512)}
			}
			return &Response{StatusCode: httpResp.StatusCode, Headers: httpResp.Header, Body: respBody}, nil
		})
		if err != nil {
			// A cancelled or expired caller context ends the call; only failures of this attempt retry.
			if ctx.Err() != nil || !canRetry || !isRetryableError(err) {
				return backoff.Permanent(err)
			}
			return err
		}
		resp = result.(*Response)
		return nil
	}, backoff.WithContext(c.retry.newBackOff(), ctx))

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.logger.Error("service request failed",
			zap.String("method", method),
			zap.String("url", url),
			zap.Error(err),
		)
		return nil, fmt.Errorf("request failed: %w", err)
	}

	c.logger.Debug("service response",
		zap.String("method", method),
		zap.String("url", url),
		zap.Int("status", resp.StatusCode),
	)

	span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", resp.StatusCode))
	} else {
		span.SetStatus(codes.Ok, "success")
	}

	return resp, nil
}

// newRequest builds one attempt's request: default headers, propagated tenant and request ID,
// then the caller's headers (which win).
func (c *Client) newRequest(ctx context.Context, method, url string, body []byte, headers map[string]string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if tid := httpware.GetTenantID(ctx); tid != "" {
		req.Header.Set(httpware.HeaderTenantID, tid)
	}
	if tslug := httpware.GetTenantSlug(ctx); tslug != "" {
		req.Header.Set(httpware.HeaderTenantSlug, tslug)
	}
	if rid := httpware.GetRequestID(ctx); rid != "" {
		req.Header.Set(httpware.HeaderRequestID, rid)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

var errResponseTooLarge = errors.New("response body exceeds the client limit")

// isIdempotent reports whether a failed call may be sent again. POST and PATCH are only retried
// when the caller supplied an idempotency key, so a retry can never charge, create or send twice.
func isIdempotent(method string, headers map[string]string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	for k := range headers {
		switch http.CanonicalHeaderKey(k) {
		case "Idempotency-Key", "X-Idempotency-Key":
			return true
		}
	}
	return false
}

// isRetryableError reports whether one attempt's failure is transient: a 5xx or 429 answer, or a
// network error (refused, reset, timeout, truncated body). Circuit-breaker rejections and
// oversized responses are final.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errResponseTooLarge) {
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// redactHeaders copies headers for logging with credentials masked.
func redactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "X-Api-Key", "Cookie", "Set-Cookie", "Proxy-Authorization":
			out[k] = []string{"[redacted]"}
		default:
			out[k] = v
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// DecodeJSON unmarshals the response body into the provided value.
func (r *Response) DecodeJSON(v interface{}) error {
	return json.Unmarshal(r.Body, v)
}

// IsSuccess returns true if status code is 2xx.
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}
