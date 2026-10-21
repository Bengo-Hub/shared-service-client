package serviceclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

func testClient(url string) *Client {
	cfg := DefaultConfig(url, "test", zap.NewNop())
	cfg.InitialInterval = time.Millisecond
	cfg.MaxInterval = 5 * time.Millisecond
	cfg.MaxElapsedTime = 2 * time.Second
	cfg.ReadyToTrip = func(gobreaker.Counts) bool { return false }
	return New(cfg)
}

func TestGetRetriesServerErrorThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Get(context.Background(), "/x", nil)
	if err != nil {
		t.Fatalf("GET should recover after two 503s: %v", err)
	}
	if !resp.IsSuccess() || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("status %d after %d calls, want success after 3", resp.StatusCode, calls)
	}
}

func TestRetryResendsTheBody(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).Put(context.Background(), "/x", map[string]string{"a": "b"}, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[1], `"a":"b"`) {
		t.Fatalf("every attempt must carry the body, got %q", bodies)
	}
}

func TestPostIsNotRetriedWithoutIdempotencyKey(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).Post(context.Background(), "/pay", map[string]int{"n": 1}, nil); err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("POST without an idempotency key was sent %d times, want 1", n)
	}
}

func TestPostWithIdempotencyKeyIsRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Post(context.Background(), "/pay", map[string]int{"n": 1},
		map[string]string{"idempotency-key": "k1"})
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("keyed POST should retry and succeed: %v %v", resp, err)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Get(context.Background(), "/missing", nil)
	if err != nil || resp.StatusCode != http.StatusNotFound || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("a 404 is an answer, not a failure: %v %v calls=%d", resp, err, calls)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testClient(srv.URL).Get(ctx, "/x", nil); err == nil {
		t.Fatal("a cancelled context must fail the call")
	}
	if n := atomic.LoadInt32(&calls); n > 1 {
		t.Fatalf("cancelled call was attempted %d times", n)
	}
}

func TestConcurrentCallsKeepIndependentBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), "/x", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestRedactHeadersMasksCredentials(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer abc")
	h.Set("X-API-Key", "secret")
	h.Set("X-Tenant-ID", "t1")
	out := redactHeaders(h)
	if out.Get("Authorization") != "[redacted]" || out.Get("X-API-Key") != "[redacted]" {
		t.Fatalf("credentials must be masked: %v", out)
	}
	if out.Get("X-Tenant-ID") != "t1" || h.Get("Authorization") != "Bearer abc" {
		t.Fatal("other headers stay and the original is untouched")
	}
}
