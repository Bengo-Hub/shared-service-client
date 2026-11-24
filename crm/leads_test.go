package crm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCaptureLead(t *testing.T) {
	tid, lid := uuid.New(), uuid.New()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != leadsPath || r.Method != http.MethodPost || r.Header.Get("X-Tenant-ID") != tid.String() || r.Header.Get("X-API-Key") != "k" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"lead":{"id":"` + lid.String() + `"},"created":false}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", nil)
	lead, err := c.CaptureLead(context.Background(), LeadInput{TenantID: tid, Source: "property_enquiry", ServiceOrigin: "maskani-api",
		Phone: "0712 345 678", FirstName: "Amina", Interest: "rent", Metadata: map[string]any{"enquiry_id": "e1"}})
	if err != nil || lead.ID != lid || lead.Created {
		t.Fatalf("lead %+v err %v", lead, err)
	}
	if got["source"] != "property_enquiry" || got["phone"] == "" || got["email"] != nil {
		t.Fatalf("body %v", got)
	}
	if _, err := c.CaptureLead(context.Background(), LeadInput{TenantID: tid}); !errors.Is(err, ErrNoIdentifier) {
		t.Fatalf("no identifier: %v", err)
	}
	if _, err := New("", "", nil).CaptureLead(context.Background(), LeadInput{TenantID: tid, Email: "a@b.co"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
}
