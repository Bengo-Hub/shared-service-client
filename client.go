package serviceclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

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
	retryBackoff   backoff.BackOff
	tracer         trace.Tracer
	logger         *zap.Logger
	serviceName    string
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

	// Configure retry backoff
	retryBackoff := backoff.NewExponentialBackOff()
	retryBackoff.InitialInterval = cfg.InitialInterval
	retryBackoff.MaxInterval = cfg.MaxInterval
	retryBackoff.MaxElapsedTime = cfg.MaxElapsedTime
	retryBackoff.Multiplier = cfg.Multiplier
	retryBackoff.RandomizationFactor = cfg.RandomizationFactor

	return &Client{
		baseURL:        cfg.BaseURL,
		httpClient:     httpClient,
		circuitBreaker: cb,
		retryBackoff:   retryBackoff,
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

	// Prepare request body
	var bodyReader io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	// Create HTTP request
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set default headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Set custom headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// Log request
	c.logger.Debug("service request",
		zap.String("method", method),
		zap.String("url", url),
		zap.Any("headers", req.Header),
	)

	// Execute with circuit breaker and retry
	var resp *Response
	err = backoff.Retry(func() error {
		// Execute through circuit breaker
		result, err := c.circuitBreaker.Execute(func() (interface{}, error) {
			httpResp, err := c.httpClient.Do(req)
			if err != nil {
				span.RecordError(err)
				return nil, err
			}

			// Read response body
			respBody, readErr := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read response: %w", readErr)
			}

			// Check if we should retry based on status code
			if httpResp.StatusCode >= 500 || httpResp.StatusCode == 429 {
				return nil, fmt.Errorf("retryable status %d: %s", httpResp.StatusCode, string(respBody))
			}

			resp = &Response{
				StatusCode: httpResp.StatusCode,
				Headers:    httpResp.Header,
				Body:       respBody,
			}

			return resp, nil
		})

		if err != nil {
			// Check if error is retryable
			if !isRetryableError(err) {
				return backoff.Permanent(err)
			}
			return err
		}

		resp = result.(*Response)
		return nil
	}, backoff.WithContext(c.retryBackoff, ctx))

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

	// Log response
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

// isRetryableError determines if an error should be retried.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Network errors are retryable
	if err.Error() == "context deadline exceeded" ||
		err.Error() == "context canceled" ||
		err.Error() == "connection refused" ||
		err.Error() == "connection reset" {
		return true
	}

	// HTTP 5xx and 429 are retryable (handled in doRequest)
	return false
}

// DecodeJSON unmarshals the response body into the provided value.
func (r *Response) DecodeJSON(v interface{}) error {
	return json.Unmarshal(r.Body, v)
}

// IsSuccess returns true if status code is 2xx.
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}
