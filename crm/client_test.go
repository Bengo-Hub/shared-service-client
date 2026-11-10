package crm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testKey = "test-service-key"

func TestMain(m *testing.M) {
	// Keep read retries fast.
	readRetryInitial = 5 * time.Millisecond
	readRetryMax = 20 * time.Millisecond
	os.Exit(m.Run())
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, testKey, nil)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestUpsertStrict_SendsNormalisedPhoneAndBothIdentifiers(t *testing.T) {
	tenant, id := uuid.New(), uuid.New()
	var got map[string]string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/contacts/upsert" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != testKey {
			t.Errorf("missing service key header")
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, 200, map[string]any{"id": id, "first_name": "Amina", "last_name": "Otieno",
			"phone": "+254712345678", "email": "amina@example.com"})
	})

	out, err := c.UpsertStrict(context.Background(), UpsertInput{
		TenantID: tenant, Phone: "0712 345 678", Email: "amina@example.com",
		FirstName: "Amina", LastName: "Otieno",
	})
	if err != nil {
		t.Fatalf("UpsertStrict: %v", err)
	}
	if out.ID != id {
		t.Errorf("id = %s, want %s", out.ID, id)
	}
	if got["phone"] != "+254712345678" {
		t.Errorf("phone sent = %q, want +254712345678", got["phone"])
	}
	if got["email"] != "amina@example.com" || got["tenant_id"] != tenant.String() {
		t.Errorf("body = %v", got)
	}
}

func TestUpsertStrict_MapsBadRequestToError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]string{"error": "email or phone required", "code": "missing_identifier"})
	})
	_, err := c.UpsertStrict(context.Background(), UpsertInput{TenantID: uuid.New(), Email: "a@b.co"})
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if ce.Status != 400 || ce.Code != "missing_identifier" || ce.Message != "email or phone required" {
		t.Errorf("error = %+v", ce)
	}
}

func TestUpsertStrict_NoIdentifier(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) })
	_, err := c.UpsertStrict(context.Background(), UpsertInput{TenantID: uuid.New(), FirstName: "x"})
	if !errors.Is(err, ErrNoIdentifier) {
		t.Fatalf("err = %v, want ErrNoIdentifier", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("no request should be sent")
	}
}

func TestUpsertStrict_UnreachableHasStatusZero(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(url, testKey, nil)
	_, err := c.UpsertStrict(context.Background(), UpsertInput{TenantID: uuid.New(), Email: "a@b.co"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 0 {
		t.Fatalf("err = %v, want *Error with Status 0", err)
	}
}

func TestUpsert_BestEffortReturnsNilOnFailure(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 500, map[string]string{"error": "boom"})
	})
	if got := c.Upsert(context.Background(), UpsertInput{TenantID: uuid.New(), Email: "a@b.co"}); got != uuid.Nil {
		t.Errorf("Upsert = %s, want Nil", got)
	}
}

func TestGet_FoundAndNotFound(t *testing.T) {
	tenant, id := uuid.New(), uuid.New()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != testKey {
			t.Errorf("missing service key header")
		}
		if r.URL.Query().Get("tenant_id") != tenant.String() {
			t.Errorf("tenant_id = %q", r.URL.Query().Get("tenant_id"))
		}
		if r.URL.Path == "/api/v1/internal/contacts/"+id.String() {
			writeJSON(w, 200, map[string]any{"id": id, "first_name": "Amina", "phone": "+254712345678"})
			return
		}
		writeJSON(w, 404, map[string]string{"error": "contact not found", "code": "not_found"})
	})

	got, err := c.Get(context.Background(), tenant, id)
	if err != nil || got.ID != id || got.FirstName != "Amina" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := c.Get(context.Background(), tenant, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestByPhone_NormalisesBeforeLookup(t *testing.T) {
	var phone string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		phone = r.URL.Query().Get("phone")
		writeJSON(w, 200, map[string]any{"id": uuid.New()})
	})
	if _, err := c.ByPhone(context.Background(), uuid.New(), "0712345678"); err != nil {
		t.Fatalf("ByPhone: %v", err)
	}
	if phone != "+254712345678" {
		t.Errorf("phone sent = %q", phone)
	}
}

func TestSearch_ClampsLimitAndDecodesData(t *testing.T) {
	var limit string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		limit = r.URL.Query().Get("limit")
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": uuid.New(), "first_name": "A"}}, "total": 1})
	})
	got, err := c.Search(context.Background(), uuid.New(), "am", 500)
	if err != nil || len(got) != 1 {
		t.Fatalf("Search = %v, %v", got, err)
	}
	if limit != "100" {
		t.Errorf("limit = %s, want 100", limit)
	}
}

func TestBatchGet_SplitsFourFiftyIdsIntoThreeCalls(t *testing.T) {
	tenant := uuid.New()
	ids := make([]uuid.UUID, 450)
	for i := range ids {
		ids[i] = uuid.New()
	}
	var calls int32
	var mu sync.Mutex
	var sizes []int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body struct {
			TenantID string      `json:"tenant_id"`
			IDs      []uuid.UUID `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.TenantID != tenant.String() {
			t.Errorf("tenant_id = %q", body.TenantID)
		}
		mu.Lock()
		sizes = append(sizes, len(body.IDs))
		mu.Unlock()
		data := make([]map[string]any, 0, len(body.IDs))
		for _, id := range body.IDs {
			data = append(data, map[string]any{"id": id, "first_name": "N"})
		}
		writeJSON(w, 200, map[string]any{"data": data})
	})

	got, err := c.BatchGet(context.Background(), tenant, ids)
	if err != nil {
		t.Fatalf("BatchGet: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 3 || sizes[0] != 200 || sizes[1] != 200 || sizes[2] != 50 {
		t.Errorf("chunk sizes = %v", sizes)
	}
	if len(got) != 450 {
		t.Errorf("result size = %d, want 450", len(got))
	}
}

func TestBatchGet_EmptyAndNilIDsSendNothing(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) })
	got, err := c.BatchGet(context.Background(), uuid.New(), []uuid.UUID{uuid.Nil})
	if err != nil || len(got) != 0 {
		t.Fatalf("BatchGet = %v, %v", got, err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("no request should be sent")
	}
}

func TestUpdate_SendsOnlyGivenFieldsAndTenant(t *testing.T) {
	tenant, id := uuid.New(), uuid.New()
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/internal/contacts/"+id.String() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, 200, map[string]any{"id": id, "first_name": "Zed"})
	})
	first, phone := "Zed", "0712345678"
	out, err := c.Update(context.Background(), tenant, id, UpdateInput{FirstName: &first, Phone: &phone})
	if err != nil || out.FirstName != "Zed" {
		t.Fatalf("Update = %+v, %v", out, err)
	}
	if got["tenant_id"] != tenant.String() || got["first_name"] != "Zed" || got["phone"] != "+254712345678" {
		t.Errorf("body = %v", got)
	}
	if _, has := got["last_name"]; has {
		t.Errorf("last_name must not be sent when nil")
	}
}

func TestDelete_SendsTenantInQuery(t *testing.T) {
	tenant, id := uuid.New(), uuid.New()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Query().Get("tenant_id") != tenant.String() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	})
	if err := c.Delete(context.Background(), tenant, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDisabledClient_ReturnsErrDisabledWithoutNetwork(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	ctx, tenant, id := context.Background(), uuid.New(), uuid.New()
	clients := map[string]*Client{
		"no url": New("", testKey, nil),
		"no key": New(srv.URL, "", nil),
		"nil":    nil,
	}
	for name, c := range clients {
		if _, err := c.UpsertStrict(ctx, UpsertInput{TenantID: tenant, Email: "a@b.co"}); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: UpsertStrict err = %v", name, err)
		}
		if got := c.Upsert(ctx, UpsertInput{TenantID: tenant, Email: "a@b.co"}); got != uuid.Nil {
			t.Errorf("%s: Upsert = %s", name, got)
		}
		if _, err := c.Get(ctx, tenant, id); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: Get err = %v", name, err)
		}
		if _, err := c.BatchGet(ctx, tenant, []uuid.UUID{id}); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: BatchGet err = %v", name, err)
		}
		if _, err := c.ByPhone(ctx, tenant, "0712345678"); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: ByPhone err = %v", name, err)
		}
		if _, err := c.Search(ctx, tenant, "x", 5); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: Search err = %v", name, err)
		}
		if _, err := c.Update(ctx, tenant, id, UpdateInput{}); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: Update err = %v", name, err)
		}
		if err := c.Delete(ctx, tenant, id); !errors.Is(err, ErrDisabled) {
			t.Errorf("%s: Delete err = %v", name, err)
		}
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("disabled clients made %d requests", calls)
	}
}

func TestSplitName(t *testing.T) {
	cases := []struct{ in, first, last string }{
		{"", "", ""},
		{"  Amina ", "Amina", ""},
		{"Amina Otieno", "Amina", "Otieno"},
		{"Amina  van der Merwe", "Amina", "van der Merwe"},
	}
	for _, tc := range cases {
		f, l := SplitName(tc.in)
		if f != tc.first || l != tc.last {
			t.Errorf("SplitName(%q) = %q, %q; want %q, %q", tc.in, f, l, tc.first, tc.last)
		}
	}
}

func TestGet_RetriesOn5xx(t *testing.T) {
	tenant, id := uuid.New(), uuid.New()
	var hits int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			writeJSON(w, 503, map[string]string{"error": "down"})
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "first_name": "Amina"})
	})
	got, err := c.Get(context.Background(), tenant, id)
	if err != nil || got.ID != id {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Errorf("hits = %d, want 3", n)
	}
}

func TestGet_5xxAfterRetriesKeepsStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 503, map[string]string{"error": "down"})
	})
	_, err := c.Get(context.Background(), uuid.New(), uuid.New())
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 503 {
		t.Fatalf("err = %v, want *Error with Status 503", err)
	}
}

func TestUpsert_WritesAreNotRetried(t *testing.T) {
	var hits int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		writeJSON(w, 500, map[string]string{"error": "boom"})
	})
	_, err := c.UpsertStrict(context.Background(), UpsertInput{TenantID: uuid.New(), Email: "a@b.co"})
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 500 {
		t.Fatalf("err = %v, want *Error with Status 500", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("hits = %d, want exactly 1", n)
	}
}

func TestGet_ContextCanceled(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"id": uuid.New()})
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Get(ctx, uuid.New(), uuid.New())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	_, err = c.UpsertStrict(ctx, UpsertInput{TenantID: uuid.New(), Email: "a@b.co"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write err = %v, want context.Canceled", err)
	}
}

func TestDelete_NotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]string{"error": "gone", "code": "not_found"})
	})
	if err := c.Delete(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNilTenantAndNilIDsRejectedClientSide(t *testing.T) {
	var hits int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) })
	ctx, id := context.Background(), uuid.New()
	if _, err := c.UpsertStrict(ctx, UpsertInput{Email: "a@b.co"}); !errors.Is(err, ErrNoTenant) {
		t.Errorf("UpsertStrict err = %v", err)
	}
	if _, err := c.Get(ctx, uuid.Nil, id); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Get err = %v", err)
	}
	if _, err := c.BatchGet(ctx, uuid.Nil, []uuid.UUID{id}); !errors.Is(err, ErrNoTenant) {
		t.Errorf("BatchGet err = %v", err)
	}
	if _, err := c.ByPhone(ctx, uuid.Nil, "0712345678"); !errors.Is(err, ErrNoTenant) {
		t.Errorf("ByPhone err = %v", err)
	}
	if _, err := c.Search(ctx, uuid.Nil, "x", 5); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Search err = %v", err)
	}
	if _, err := c.Update(ctx, uuid.Nil, id, UpdateInput{}); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Update err = %v", err)
	}
	if err := c.Delete(ctx, uuid.Nil, id); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Delete err = %v", err)
	}
	tenant := uuid.New()
	if _, err := c.Get(ctx, tenant, uuid.Nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get nil id err = %v", err)
	}
	if _, err := c.ByPhone(ctx, tenant, "  "); !errors.Is(err, ErrNotFound) {
		t.Errorf("ByPhone empty err = %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("rejected calls made %d requests", n)
	}
}
