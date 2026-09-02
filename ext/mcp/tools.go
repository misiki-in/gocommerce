package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/misiki/gocommerce/core"
)

// builtinTools are the store operations an agent can perform.
//
// Each one calls a domain service — the same one the REST API calls — so an
// agent cannot reach a state a person could not, and cannot skip a rule by
// coming in through a different door.
func (m *Module) builtinTools() []Tool {
	return []Tool{
		{
			Name: "store_info",
			Description: "Describe this store: its currency, languages and the " +
				"payment and fulfillment methods installed.",
			Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
				cfg := m.app.Config()
				return map[string]any{
					"currency":              cfg.Currency,
					"default_language":      cfg.DefaultLanguage,
					"languages":             cfg.Languages,
					"payment_methods":       m.app.Pay().Methods(),
					"fulfillment_providers": m.app.Ship().Providers(),
					"engine_version":        gocommerce.Version,
				}, nil
			},
		},
		{
			Name: "store_health",
			Description: "Run operational diagnostics: database, migrations, admin " +
				"access, the event outbox, stock reservations, carts, catalog and " +
				"the API contract. Each check reports ok, warn or fail with a hint. " +
				"Call this first when something is behaving oddly.",
			Call: func(ctx context.Context, _ json.RawMessage) (any, error) {
				// The same report `gocommerce doctor` renders. An agent asked to
				// diagnose a store should not have to infer health from a dozen
				// separate reads, and must never reach for SQL to do it.
				return m.app.Diagnose(ctx), nil
			},
		},
		{
			Name:        "list_products",
			Description: "List products in the catalog, optionally filtered by a search term.",
			InputSchema: object(props{
				"query":  str("Match against title and description."),
				"status": enumStr("Filter by status.", "draft", "active", "archived"),
				"limit":  integer("How many to return (default 20, max 200)."),
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Query  string `json:"query"`
					Status string `json:"status"`
					Limit  int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				products, total, err := m.app.Products().ListProducts(ctx, gocommerce.ProductQuery{
					Search: args.Query, Status: args.Status,
					Limit: limitOr(args.Limit, 20),
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"total": total, "products": summarizeProducts(products)}, nil
			},
		},
		{
			Name:        "get_product",
			Description: "Get one product with all of its variants and stock levels.",
			InputSchema: object(props{"id": integer("The product id.")}, "id"),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				return m.app.Products().GetProduct(ctx, args.ID)
			},
		},
		{
			Name: "list_low_stock_variants",
			Description: "List sellable variants at or below a stock threshold — " +
				"what needs reordering.",
			InputSchema: object(props{
				"threshold": integer("Available units at or below this count (default 5)."),
				"limit":     integer("How many to return (default 50)."),
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Threshold *int `json:"threshold"`
					Limit     int  `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				threshold := 5
				if args.Threshold != nil {
					threshold = *args.Threshold
				}
				variants, total, err := m.app.Stock().LowStock(ctx, threshold, limitOr(args.Limit, 50), 0)
				if err != nil {
					return nil, err
				}
				return map[string]any{"total": total, "variants": summarizeVariants(variants)}, nil
			},
		},
		{
			Name:        "list_orders",
			Description: "List orders, most recent first, optionally filtered.",
			InputSchema: object(props{
				"status":         enumStr("Order status.", "pending", "confirmed", "shipped", "delivered", "cancelled"),
				"payment_status": enumStr("Payment status.", "pending", "paid", "failed", "refunded"),
				"email":          str("Filter by customer email."),
				"limit":          integer("How many to return (default 20, max 200)."),
			}),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					Status        string `json:"status"`
					PaymentStatus string `json:"payment_status"`
					Email         string `json:"email"`
					Limit         int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				orders, total, err := m.app.Order().List(ctx, gocommerce.OrderQuery{
					Status: args.Status, PaymentStatus: args.PaymentStatus,
					Email: args.Email, Limit: limitOr(args.Limit, 20),
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"total": total, "orders": summarizeOrders(orders)}, nil
			},
		},
		{
			Name:        "get_order",
			Description: "Get one order in full, including its lines and shipments.",
			InputSchema: object(props{"id": integer("The order id.")}, "id"),
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				order, err := m.app.Order().Get(ctx, args.ID)
				if err != nil {
					return nil, err
				}
				// The access token is the customer's credential, not something
				// an agent needs in order to read the order.
				order.AccessToken = ""
				return order, nil
			},
		},
		{
			Name: "update_variant_inventory",
			Description: "Change a variant's stock. Use adjust to move it by a delta " +
				"(receiving stock) or set to replace it (a stock take). Stock cannot " +
				"go below what is already reserved for open orders.",
			InputSchema: object(props{
				"variant_id": integer("The variant id."),
				"adjust":     integer("Move the on-hand count by this much."),
				"set":        integer("Replace the on-hand count with this."),
			}, "variant_id"),
			Mutates: true,
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					VariantID int64 `json:"variant_id"`
					Adjust    *int  `json:"adjust"`
					Set       *int  `json:"set"`
					// Omitted means the default location, which is the only
					// one most stores have and the one an agent that has not
					// been told about locations should be moving.
					LocationID int64 `json:"location_id"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				switch {
				case args.Adjust != nil && args.Set != nil:
					return nil, fmt.Errorf("send either adjust or set, not both")
				case args.Adjust != nil:
					return m.app.Stock().Adjust(ctx, args.VariantID, args.LocationID, *args.Adjust)
				case args.Set != nil:
					return m.app.Stock().SetOnHand(ctx, args.VariantID, args.LocationID, *args.Set)
				default:
					return nil, fmt.Errorf("send either adjust or set")
				}
			},
		},
		{
			Name: "mark_order_paid",
			Description: "Record that an order has been paid — how cash on delivery is " +
				"settled. This also confirms the order so it can be shipped.",
			InputSchema: object(props{
				"order_id":  integer("The order id."),
				"reference": str("The payment reference, if there is one."),
			}, "order_id"),
			Mutates: true,
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					OrderID   int64  `json:"order_id"`
					Reference string `json:"reference"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				return m.app.Pay().MarkPaid(ctx, args.OrderID, args.Reference)
			},
		},
		{
			Name: "cancel_order",
			Description: "Cancel an order and return its stock. An order that has already " +
				"shipped cannot be cancelled — that is a return.",
			InputSchema: object(props{
				"order_id": integer("The order id."),
				"reason":   str("Why it is being cancelled."),
			}, "order_id"),
			Mutates: true,
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					OrderID int64  `json:"order_id"`
					Reason  string `json:"reason"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				return m.app.Order().Cancel(ctx, args.OrderID, args.Reason)
			},
		},
		{
			Name:        "create_fulfillment",
			Description: "Ship a confirmed order, recording a tracking number.",
			InputSchema: object(props{
				"order_id": integer("The order id."),
				"provider": str("Fulfillment provider code; defaults to manual."),
				"tracking": str("The tracking number, for manual fulfillment."),
			}, "order_id"),
			Mutates: true,
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					OrderID  int64  `json:"order_id"`
					Provider string `json:"provider"`
					Tracking string `json:"tracking"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				return m.app.Ship().Create(ctx, args.OrderID, args.Provider,
					gocommerce.ShipRequest{Tracking: args.Tracking})
			},
		},
		{
			Name:        "mark_order_delivered",
			Description: "Record that a shipped order reached the customer.",
			InputSchema: object(props{"order_id": integer("The order id.")}, "order_id"),
			Mutates:     true,
			Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var args struct {
					OrderID int64 `json:"order_id"`
				}
				if err := decode(raw, &args); err != nil {
					return nil, err
				}
				return m.app.Order().MarkDelivered(ctx, args.OrderID)
			},
		},
	}
}

// ------------------------------------------------------------------ summaries

// The list tools return summaries rather than whole records. An agent reading
// fifty orders does not need every field of each, and a smaller payload is a
// cheaper and clearer one.

func summarizeProducts(products []*gocommerce.Product) []map[string]any {
	out := make([]map[string]any, 0, len(products))
	for _, p := range products {
		variants := make([]map[string]any, 0, len(p.Variants))
		for _, v := range p.Variants {
			variants = append(variants, map[string]any{
				"id": v.ID, "sku": v.SKU, "label": v.Label,
				"price_minor": v.Price.AmountMinor, "available": v.Available,
			})
		}
		out = append(out, map[string]any{
			"id": p.ID, "title": p.Title, "slug": p.Slug,
			"status": p.Status, "variants": variants,
		})
	}
	return out
}

func summarizeVariants(variants []*gocommerce.Variant) []map[string]any {
	out := make([]map[string]any, 0, len(variants))
	for _, v := range variants {
		out = append(out, map[string]any{
			"id": v.ID, "product_id": v.ProductID, "sku": v.SKU, "label": v.Label,
			"on_hand": v.StockOnHand, "reserved": v.StockReserved, "available": v.Available,
		})
	}
	return out
}

func summarizeOrders(orders []*gocommerce.Order) []map[string]any {
	out := make([]map[string]any, 0, len(orders))
	for _, o := range orders {
		out = append(out, map[string]any{
			"id": o.ID, "number": o.Number, "status": o.Status,
			"payment_status": o.PaymentStatus, "payment_method": o.PaymentProvider,
			"total_minor": o.Total.AmountMinor, "currency": o.Currency,
			"email": o.Email, "items": len(o.Lines),
			"created_at": o.CreatedAt,
		})
	}
	return out
}

// ------------------------------------------------------------ schema helpers

type props map[string]any

func object(p props, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": map[string]any(p)}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integer(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func enumStr(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("could not read the arguments: %w", err)
	}
	return nil
}

func limitOr(requested, fallback int) int {
	if requested <= 0 {
		return fallback
	}
	if requested > gocommerce.MaxLimit {
		return gocommerce.MaxLimit
	}
	return requested
}
