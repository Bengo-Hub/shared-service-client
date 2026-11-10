// Package crm is the shared S2S client for the MarketFlow CRM, the system of record for
// customer identity. Services keep only a contact id and call this client for name, email
// and phone, so no service holds its own copy of personal data.
//
// Reads (Get, ByPhone, Search) go through the module's retrying client with a circuit
// breaker. Writes (upsert, update, delete, batch lookup) are sent once and never retried.
package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/httpware/contact"
	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/google/uuid"
	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

const (
	// callTimeout bounds every call, including the retries of a read.
	callTimeout = 5 * time.Second
	// batchChunk is the most ids marketflow accepts in one batch call.
	batchChunk = 200
	// maxSearchLimit is the largest page marketflow returns for a search.
	maxSearchLimit = 100
	// defaultSearchLimit is used when the caller passes no limit.
	defaultSearchLimit = 20

	contactsPath = "/api/v1/internal/contacts"

	// CodeCircuitOpen is the Error.Code when the circuit breaker rejected the call.
	CodeCircuitOpen = "circuit_open"
)

// Read retry backoff. Variables so tests can shorten them.
var (
	readRetryInitial = 100 * time.Millisecond
	readRetryMax     = time.Second
)

// ErrNoTenant means a call was made with a nil tenant id.
var ErrNoTenant = errors.New("crm: tenant id is required")

// ErrDisabled means the client has no URL or service key, so no call is made.
var ErrDisabled = errors.New("crm: not configured")

// ErrNoIdentifier means an upsert had neither an email nor a phone, which the CRM keys on.
var ErrNoIdentifier = errors.New("crm: an email or phone is required")

// ErrNotFound means the CRM answered 404 for the contact.
var ErrNotFound = errors.New("crm: contact not found")

// Error is a failed CRM call. Status is the CRM's HTTP status, or 0 when no answer was
// received: the CRM was unreachable, or the circuit breaker rejected the call (Code is then
// CodeCircuitOpen). Code and Message come from marketflow's {"error", "code"} body when it
// sent one. Err holds the transport error, when there is one, for errors.Is and errors.As.
type Error struct {
	Status  int
	Code    string
	Message string
	Err     error
}

// Unwrap exposes the transport error.
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Error() string {
	if e.Status == 0 {
		return "crm: unreachable: " + e.Message
	}
	if e.Code != "" {
		return fmt.Sprintf("crm: status %d (%s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("crm: status %d: %s", e.Status, e.Message)
}

// Contact is the identity a CRM contact exposes to other services.
type Contact struct {
	ID        uuid.UUID `json:"id"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	Email     string    `json:"email"`
	Phone     string    `json:"phone"`
}

// UpsertInput is the create-or-get payload. Phone is normalised inside the client.
type UpsertInput struct {
	TenantID  uuid.UUID
	Phone     string
	Email     string
	FirstName string
	LastName  string
}

// UpdateInput is a partial edit: a nil field is left unchanged.
type UpdateInput struct {
	FirstName *string
	LastName  *string
	Email     *string
	Phone     *string
}

// Client calls the MarketFlow internal (X-API-Key) contacts API.
type Client struct {
	baseURL    string
	serviceKey string
	reader     *serviceclient.Client // retrying client, GET only
	writer     *http.Client          // single attempt, writes
	log        *zap.Logger
}

// New builds a CRM client. An empty baseURL or serviceKey disables it: every call returns
// ErrDisabled without touching the network. Every method is also safe on a nil *Client.
func New(baseURL, serviceKey string, log *zap.Logger) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		serviceKey: serviceKey,
		writer:     &http.Client{Timeout: callTimeout},
		log:        log,
	}
	if c.enabled() {
		cfg := serviceclient.DefaultConfig(c.baseURL, "marketflow-crm", log)
		cfg.Timeout = callTimeout
		cfg.MaxElapsedTime = callTimeout
		cfg.InitialInterval = readRetryInitial
		cfg.MaxInterval = readRetryMax
		c.reader = serviceclient.New(cfg)
	}
	return c
}

func (c *Client) enabled() bool {
	return c != nil && c.baseURL != "" && c.serviceKey != ""
}

// UpsertStrict creates or returns the contact for the given identifiers and reports every
// failure: ErrDisabled, ErrNoIdentifier, or a *Error carrying the CRM's status and code.
func (c *Client) UpsertStrict(ctx context.Context, in UpsertInput) (Contact, error) {
	if !c.enabled() {
		return Contact{}, ErrDisabled
	}
	if in.TenantID == uuid.Nil {
		return Contact{}, ErrNoTenant
	}
	email := strings.TrimSpace(in.Email)
	phone := normalisePhone(in.Phone)
	if email == "" && phone == "" {
		return Contact{}, ErrNoIdentifier
	}
	body := map[string]string{
		"tenant_id":  in.TenantID.String(),
		"phone":      phone,
		"email":      email,
		"first_name": strings.TrimSpace(in.FirstName),
		"last_name":  strings.TrimSpace(in.LastName),
	}
	var out Contact
	if err := c.write(ctx, http.MethodPost, contactsPath+"/upsert", body, &out); err != nil {
		return Contact{}, err
	}
	return out, nil
}

// Upsert is the best-effort form of UpsertStrict. It returns the contact id, or uuid.Nil on
// any failure, which it logs. Callers must not fail their own work on a CRM error.
func (c *Client) Upsert(ctx context.Context, in UpsertInput) uuid.UUID {
	got, err := c.UpsertStrict(ctx, in)
	if err != nil {
		if c != nil && !errors.Is(err, ErrDisabled) && !errors.Is(err, ErrNoIdentifier) {
			c.log.Warn("crm upsert failed", zap.Error(err))
		}
		return uuid.Nil
	}
	return got.ID
}

// Get returns one contact, or ErrNotFound.
func (c *Client) Get(ctx context.Context, tenantID, id uuid.UUID) (Contact, error) {
	if !c.enabled() {
		return Contact{}, ErrDisabled
	}
	if tenantID == uuid.Nil {
		return Contact{}, ErrNoTenant
	}
	if id == uuid.Nil {
		return Contact{}, ErrNotFound
	}
	q := url.Values{"tenant_id": {tenantID.String()}}
	var out Contact
	if err := c.read(ctx, contactsPath+"/"+id.String(), q, &out); err != nil {
		return Contact{}, err
	}
	return out, nil
}

// ByPhone finds a contact by phone, normalised first, or returns ErrNotFound.
func (c *Client) ByPhone(ctx context.Context, tenantID uuid.UUID, phone string) (Contact, error) {
	if !c.enabled() {
		return Contact{}, ErrDisabled
	}
	if tenantID == uuid.Nil {
		return Contact{}, ErrNoTenant
	}
	phone = normalisePhone(phone)
	if phone == "" {
		return Contact{}, ErrNotFound
	}
	q := url.Values{"tenant_id": {tenantID.String()}, "phone": {phone}}
	var out Contact
	if err := c.read(ctx, contactsPath+"/by-phone", q, &out); err != nil {
		return Contact{}, err
	}
	return out, nil
}

// Search returns contacts whose name, email or phone match q. limit is clamped to 1..100
// and defaults to 20.
func (c *Client) Search(ctx context.Context, tenantID uuid.UUID, q string, limit int) ([]Contact, error) {
	if !c.enabled() {
		return nil, ErrDisabled
	}
	if tenantID == uuid.Nil {
		return nil, ErrNoTenant
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	query := url.Values{
		"tenant_id": {tenantID.String()},
		"q":         {q},
		"limit":     {strconv.Itoa(limit)},
	}
	var out struct {
		Data []Contact `json:"data"`
	}
	if err := c.read(ctx, contactsPath+"/search", query, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// BatchGet resolves many ids at once, in chunks of 200. Ids the CRM does not know are
// absent from the map. Nil and repeated ids are skipped. It stops at the first failed chunk.
func (c *Client) BatchGet(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Contact, error) {
	if !c.enabled() {
		return nil, ErrDisabled
	}
	if tenantID == uuid.Nil {
		return nil, ErrNoTenant
	}
	seen := make(map[uuid.UUID]struct{}, len(ids))
	unique := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	result := make(map[uuid.UUID]Contact, len(unique))
	for start := 0; start < len(unique); start += batchChunk {
		end := start + batchChunk
		if end > len(unique) {
			end = len(unique)
		}
		body := map[string]any{"tenant_id": tenantID.String(), "ids": unique[start:end]}
		var out struct {
			Data []Contact `json:"data"`
		}
		if err := c.write(ctx, http.MethodPost, contactsPath+"/batch", body, &out); err != nil {
			return nil, err
		}
		for _, ct := range out.Data {
			result[ct.ID] = ct
		}
	}
	return result, nil
}

// Update edits a contact's identity fields. Only non-nil fields are sent. A phone is
// normalised first.
func (c *Client) Update(ctx context.Context, tenantID, id uuid.UUID, in UpdateInput) (Contact, error) {
	if !c.enabled() {
		return Contact{}, ErrDisabled
	}
	if tenantID == uuid.Nil {
		return Contact{}, ErrNoTenant
	}
	if id == uuid.Nil {
		return Contact{}, ErrNotFound
	}
	body := map[string]any{"tenant_id": tenantID.String()}
	if in.FirstName != nil {
		body["first_name"] = *in.FirstName
	}
	if in.LastName != nil {
		body["last_name"] = *in.LastName
	}
	if in.Email != nil {
		body["email"] = strings.TrimSpace(*in.Email)
	}
	if in.Phone != nil {
		body["phone"] = normalisePhone(*in.Phone)
	}
	var out Contact
	if err := c.write(ctx, http.MethodPut, contactsPath+"/"+id.String(), body, &out); err != nil {
		return Contact{}, err
	}
	return out, nil
}

// Delete removes a contact. The caller must first check that its own data allows it, since
// the CRM cannot see other services' records.
func (c *Client) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if !c.enabled() {
		return ErrDisabled
	}
	if tenantID == uuid.Nil {
		return ErrNoTenant
	}
	if id == uuid.Nil {
		return ErrNotFound
	}
	path := contactsPath + "/" + id.String() + "?tenant_id=" + url.QueryEscape(tenantID.String())
	return c.write(ctx, http.MethodDelete, path, nil, nil)
}

// SplitName splits a free-form name into first and last name.
func SplitName(full string) (first, last string) {
	parts := strings.Fields(full)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

// normalisePhone returns E.164 when the phone is valid. An unparseable value is passed on
// trimmed, so no data is lost and the CRM decides what to do with it.
func normalisePhone(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if n, err := contact.NormalizePhone(raw, contact.DefaultRegion); err == nil {
		return n
	}
	return raw
}

// read does a GET through the retrying client and decodes a 2xx body into out.
func (c *Client) read(ctx context.Context, path string, q url.Values, out any) error {
	if !c.enabled() {
		return ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	resp, err := c.reader.Get(ctx, path+"?"+q.Encode(), map[string]string{httpware.HeaderAPIKey: c.serviceKey})
	if err != nil {
		return transportError(ctx, err)
	}
	if !resp.IsSuccess() {
		return failure(resp.StatusCode, resp.Body)
	}
	if err := resp.DecodeJSON(out); err != nil {
		return fmt.Errorf("crm: decode response: %w", err)
	}
	return nil
}

// write sends one request with no retry and decodes a 2xx body into out when out is set.
func (c *Client) write(ctx context.Context, method, path string, body any, out any) error {
	if !c.enabled() {
		return ErrDisabled
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("crm: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(httpware.HeaderAPIKey, c.serviceKey)

	resp, err := c.writer.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failure(resp.StatusCode, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("crm: decode response: %w", err)
	}
	return nil
}

// transportError maps a call that got no usable answer. A cancelled or expired context is
// returned as its own error so errors.Is(err, context.Canceled) works. A 5xx left after
// retries keeps its real status, a rejection by the circuit breaker gets CodeCircuitOpen,
// and anything else is an unreachable CRM (Status 0).
func transportError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("crm: %w", cerr)
	}
	if status, ok := serviceclient.StatusCode(err); ok {
		return &Error{Status: status, Message: err.Error(), Err: err}
	}
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return &Error{Code: CodeCircuitOpen, Message: err.Error(), Err: err}
	}
	return &Error{Message: err.Error(), Err: err}
}

// failure maps a non-2xx answer to ErrNotFound or a *Error.
func failure(status int, body []byte) error {
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	var f struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &f)
	msg := f.Error
	if msg == "" {
		msg = f.Code
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &Error{Status: status, Code: f.Code, Message: msg}
}
