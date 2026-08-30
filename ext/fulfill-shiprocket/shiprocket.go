// Package shiprocket books shipments through Shiprocket.
//
// Registering it adds "shiprocket" as a fulfillment provider, so an operator
// ships by posting to the engine's own /api/admin/create-fulfillment with
// `"provider": "shiprocket"` — the engine still owns the order's state and its
// events, and this module only talks to the carrier.
//
//	app, err := gocommerce.New(cfg,
//	    shiprocket.New(shiprocket.Config{
//	        Email:    os.Getenv("SHIPROCKET_EMAIL"),
//	        Password: os.Getenv("SHIPROCKET_PASSWORD"),
//	        PickupLocation: "Primary",
//	    }),
//	)
package shiprocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misiki/gocommerce"
)

const (
	defaultBaseURL = "https://apiv2.shiprocket.in"
	// Shiprocket tokens last ten days; refreshing a day early avoids the
	// awkward case of a token expiring between two calls of one shipment.
	tokenLifetime = 9 * 24 * time.Hour
)

// Config configures the module.
type Config struct {
	// Email and Password are the Shiprocket API user's credentials. Required.
	Email, Password string
	// PickupLocation is the nickname of the pickup address registered in
	// Shiprocket. Required — the carrier has to collect the parcel somewhere.
	PickupLocation string
	// DefaultWeightKg is used for a variant with no weight recorded.
	DefaultWeightKg float64
	// DefaultDimensionsCm is used for a parcel with no dimensions.
	DefaultLengthCm, DefaultBreadthCm, DefaultHeightCm float64
	// BaseURL overrides the endpoint, for tests.
	BaseURL string
	// Client overrides the HTTP client.
	Client *http.Client
}

// Module is the Shiprocket fulfillment provider.
type Module struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger

	mu        sync.Mutex
	token     string
	tokenTime time.Time
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "fulfill-shiprocket" }

// Migrations implements gocommerce.Module. The engine already stores the
// shipment; this module keeps no state of its own.
func (m *Module) Migrations() []gocommerce.Migration { return nil }

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	switch {
	case strings.TrimSpace(m.cfg.Email) == "":
		return errors.New("shiprocket: Email is required")
	case strings.TrimSpace(m.cfg.Password) == "":
		return errors.New("shiprocket: Password is required")
	case strings.TrimSpace(m.cfg.PickupLocation) == "":
		return errors.New("shiprocket: PickupLocation is required — the carrier has to collect the parcel somewhere")
	}
	if m.cfg.BaseURL == "" {
		m.cfg.BaseURL = defaultBaseURL
	}
	if m.cfg.DefaultWeightKg <= 0 {
		m.cfg.DefaultWeightKg = 0.5
	}
	if m.cfg.DefaultLengthCm <= 0 {
		m.cfg.DefaultLengthCm, m.cfg.DefaultBreadthCm, m.cfg.DefaultHeightCm = 15, 15, 10
	}
	m.client = m.cfg.Client
	if m.client == nil {
		m.client = &http.Client{Timeout: 30 * time.Second}
	}
	m.log = app.Log()

	app.RegisterFulfillment(m)
	return nil
}

// Code implements gocommerce.FulfillmentProvider.
func (m *Module) Code() string { return "shiprocket" }

// Ship creates the order in Shiprocket and assigns a waybill.
//
// It runs before the engine opens its transaction, so a carrier having a slow
// minute never holds a lock on the orders table.
func (m *Module) Ship(ctx context.Context, order *gocommerce.Order, req gocommerce.ShipRequest) (gocommerce.Shipment, error) {
	shipmentID, err := m.createOrder(ctx, order, req)
	if err != nil {
		return gocommerce.Shipment{}, err
	}

	awb, label, err := m.assignAWB(ctx, shipmentID, req.Meta["courier_id"])
	if err != nil {
		// The order exists in Shiprocket but has no waybill. Say so precisely:
		// the operator needs to know the parcel is half-booked rather than
		// not booked at all.
		return gocommerce.Shipment{}, fmt.Errorf(
			"shiprocket: order %s was created (shipment %d) but no waybill could be assigned: %w",
			order.Number, shipmentID, err)
	}

	return gocommerce.Shipment{
		Provider: m.Code(),
		Tracking: awb,
		LabelURL: label,
	}, nil
}

func (m *Module) createOrder(ctx context.Context, order *gocommerce.Order, req gocommerce.ShipRequest) (int64, error) {
	addr := order.Address
	name := strings.TrimSpace(order.Name)
	if name == "" {
		name = strings.TrimSpace(addr.Name)
	}
	if name == "" {
		name = "Customer"
	}
	first, last := splitName(name)

	items := make([]map[string]any, 0, len(order.Lines))
	var units int
	for _, line := range order.Lines {
		items = append(items, map[string]any{
			"name":          line.Title,
			"sku":           line.SKU,
			"units":         line.Quantity,
			"selling_price": float64(line.UnitPrice.AmountMinor) / 100,
		})
		units += line.Quantity
	}

	payload := map[string]any{
		"order_id":              order.Number,
		"order_date":            order.CreatedAt.UTC().Format("2006-01-02 15:04"),
		"pickup_location":       m.cfg.PickupLocation,
		"billing_customer_name": first,
		"billing_last_name":     last,
		"billing_address":       addr.Line1,
		"billing_address_2":     addr.Line2,
		"billing_city":          addr.City,
		"billing_pincode":       addr.PostalCode,
		"billing_state":         addr.State,
		"billing_country":       addr.Country,
		"billing_email":         order.Email,
		"billing_phone":         firstNonEmpty(order.Phone, addr.Phone),
		"shipping_is_billing":   true,
		"order_items":           items,
		"payment_method":        paymentMethodFor(order),
		"sub_total":             float64(order.Subtotal.AmountMinor) / 100,
		"length":                m.dimension(req.Meta, "length_cm", m.cfg.DefaultLengthCm),
		"breadth":               m.dimension(req.Meta, "breadth_cm", m.cfg.DefaultBreadthCm),
		"height":                m.dimension(req.Meta, "height_cm", m.cfg.DefaultHeightCm),
		"weight":                m.dimension(req.Meta, "weight_kg", m.cfg.DefaultWeightKg*float64(max(units, 1))),
	}

	var created struct {
		OrderID    int64  `json:"order_id"`
		ShipmentID int64  `json:"shipment_id"`
		Status     string `json:"status"`
		Message    string `json:"message"`
	}
	if err := m.post(ctx, "/v1/external/orders/create/adhoc", payload, &created); err != nil {
		return 0, err
	}
	if created.ShipmentID == 0 {
		return 0, fmt.Errorf("shiprocket: no shipment id returned (%s)",
			firstNonEmpty(created.Message, created.Status, "unknown reason"))
	}
	return created.ShipmentID, nil
}

func (m *Module) assignAWB(ctx context.Context, shipmentID int64, courierID string) (awb, label string, err error) {
	payload := map[string]any{"shipment_id": shipmentID}
	if courierID != "" {
		// The operator asked for a specific courier via the ship request's
		// meta, which is exactly what that pass-through is for.
		if id, convErr := strconv.Atoi(courierID); convErr == nil {
			payload["courier_id"] = id
		}
	}

	var assigned struct {
		Response struct {
			Data struct {
				AWBCode string `json:"awb_code"`
				Courier string `json:"courier_name"`
			} `json:"data"`
		} `json:"response"`
		Message string `json:"message"`
	}
	if err := m.post(ctx, "/v1/external/courier/assign/awb", payload, &assigned); err != nil {
		return "", "", err
	}
	if assigned.Response.Data.AWBCode == "" {
		return "", "", fmt.Errorf("shiprocket: no waybill assigned (%s)",
			firstNonEmpty(assigned.Message, "unknown reason"))
	}

	var labelResp struct {
		LabelURL string `json:"label_url"`
	}
	if err := m.post(ctx, "/v1/external/courier/generate/label",
		map[string]any{"shipment_id": []int64{shipmentID}}, &labelResp); err != nil {
		// A missing label is inconvenient, not fatal: the parcel is booked and
		// the label can be printed from Shiprocket's dashboard.
		m.log.Warn("could not generate a Shiprocket label", "shipment_id", shipmentID, "error", err)
	}
	return assigned.Response.Data.AWBCode, labelResp.LabelURL, nil
}

// paymentMethodFor tells the carrier whether to collect money on delivery.
// Getting this wrong means either a courier who does not ask for payment, or
// one who asks a customer who has already paid.
func paymentMethodFor(order *gocommerce.Order) string {
	if order.PaymentStatus == gocommerce.PaymentPaid {
		return "Prepaid"
	}
	return "COD"
}

func (m *Module) dimension(meta map[string]string, key string, fallback float64) float64 {
	if v, ok := meta[key]; ok {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

// ------------------------------------------------------------------- transport

// authToken returns a valid bearer token, logging in when necessary.
func (m *Module) authToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && time.Since(m.tokenTime) < tokenLifetime {
		return m.token, nil
	}

	payload := map[string]string{"email": m.cfg.Email, "password": m.cfg.Password}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.cfg.BaseURL+"/v1/external/auth/login", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("shiprocket: log in: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("shiprocket: log in returned %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", errors.New("shiprocket: log in returned no token")
	}
	m.token, m.tokenTime = out.Token, time.Now()
	return m.token, nil
}

func (m *Module) post(ctx context.Context, path string, payload any, out any) error {
	token, err := m.authToken(ctx)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("shiprocket: %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("shiprocket: %s: read response: %w", path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The cached token went stale; drop it so the next attempt logs in.
		m.mu.Lock()
		m.token = ""
		m.mu.Unlock()
		return fmt.Errorf("shiprocket: %s: authentication rejected", path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Message != "" {
			return fmt.Errorf("shiprocket: %s: %s", path, apiErr.Message)
		}
		return fmt.Errorf("shiprocket: %s returned %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func splitName(full string) (first, last string) {
	parts := strings.Fields(full)
	switch len(parts) {
	case 0:
		return "Customer", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.Join(parts[1:], " ")
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
