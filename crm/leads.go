package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
)

// MarketFlow owns leads as well as contacts. A service that captures an enquiry (a property
// enquiry, a website form) hands it to MarketFlow and keeps at most the lead id, never its own
// copy of the person. MarketFlow keeps one lead per person per tenant: a second enquiry from the
// same email or phone is recorded on the existing lead and Created is false.

const leadsPath = "/api/v1/internal/leads"

// LeadInput is one enquiry for MarketFlow. Phone is normalised inside the client.
type LeadInput struct {
	TenantID uuid.UUID
	// Source is the lead source MarketFlow records ("property_enquiry", "website_contact").
	Source string
	// ServiceOrigin names the calling service ("maskani-api").
	ServiceOrigin string
	Phone         string
	Email         string
	FirstName     string
	LastName      string
	// Interest is what the person asked about, in their words or the form's topic.
	Interest string
	// Metadata holds the service's own references (property, unit, enquiry id) and the
	// person's consent record.
	Metadata map[string]any
}

// Lead is what MarketFlow answers for a captured enquiry.
type Lead struct {
	ID uuid.UUID `json:"id"`
	// Created is false when the person was already a lead of the tenant.
	Created bool `json:"-"`
}

// CaptureLead sends an enquiry to MarketFlow (POST /internal/leads). It is a single attempt: a
// failure is reported and the caller decides whether to retry later.
func (c *Client) CaptureLead(ctx context.Context, in LeadInput) (Lead, error) {
	if !c.enabled() {
		return Lead{}, ErrDisabled
	}
	if in.TenantID == uuid.Nil {
		return Lead{}, ErrNoTenant
	}
	email := strings.TrimSpace(in.Email)
	phone := normalisePhone(in.Phone)
	if email == "" && phone == "" {
		return Lead{}, ErrNoIdentifier
	}
	body := map[string]any{
		"service_origin": in.ServiceOrigin,
		"source":         in.Source,
		"metadata":       in.Metadata,
	}
	for k, v := range map[string]string{"email": email, "phone": phone, "first_name": strings.TrimSpace(in.FirstName),
		"last_name": strings.TrimSpace(in.LastName), "interest": strings.TrimSpace(in.Interest)} {
		if v != "" {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Lead{}, fmt.Errorf("crm: encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+leadsPath, bytes.NewReader(raw))
	if err != nil {
		return Lead{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(httpware.HeaderAPIKey, c.serviceKey)
	req.Header.Set("X-Tenant-ID", in.TenantID.String())
	resp, err := c.writer.Do(req)
	if err != nil {
		return Lead{}, transportError(ctx, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Lead{}, failure(resp.StatusCode, data)
	}
	var out struct {
		Lead    Lead `json:"lead"`
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Lead{}, fmt.Errorf("crm: decode response: %w", err)
	}
	out.Lead.Created = out.Created
	return out.Lead, nil
}
