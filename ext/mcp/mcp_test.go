package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/misiki/gocommerce"
	"github.com/misiki/gocommerce/gctest"
)

// rpc sends one JSON-RPC call to the MCP endpoint and returns the result.
func rpc(t *testing.T, app *gocommerce.App, method string, params any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	rec := gctest.AdminRequest(t, app, http.MethodPost, "/api/admin/x/mcp", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d: %s", method, rec.Code, rec.Body)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode: %v (body %s)", method, err, rec.Body)
	}
	if resp.Error != nil {
		t.Fatalf("%s: rpc error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result
}

// callTool runs a tool and returns its text content, plus whether the tool
// reported a domain error.
func callTool(t *testing.T, app *gocommerce.App, name string, args map[string]any) (string, bool) {
	t.Helper()
	result := rpc(t, app, "tools/call", map[string]any{"name": name, "arguments": args})

	isError, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return "", isError
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, isError
}

func TestEndpointRequiresAdminToken(t *testing.T) {
	app := gctest.New(t, New(Config{}))

	// The module writes no authentication of its own: mounting through
	// HandleAdmin is what protects it.
	rec := gctest.Request(t, app, http.MethodPost, "/api/admin/x/mcp",
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}
}

func TestInitializeAndToolsList(t *testing.T) {
	app := gctest.New(t, New(Config{ServerName: "example-store"}))

	init := rpc(t, app, "initialize", map[string]any{"protocolVersion": protocolVersion})
	if got := init["protocolVersion"]; got != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", got, protocolVersion)
	}
	server, _ := init["serverInfo"].(map[string]any)
	if server["name"] != "example-store" {
		t.Errorf("server name = %v, want example-store", server["name"])
	}

	list := rpc(t, app, "tools/list", nil)
	tools, _ := list["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("tool %q has no inputSchema; an agent cannot call it safely", name)
		}
	}
	for _, want := range []string{
		"store_info", "list_products", "get_product", "list_orders", "get_order",
		"list_low_stock_variants", "update_variant_inventory",
		"mark_order_paid", "cancel_order", "create_fulfillment", "mark_order_delivered",
	} {
		if !names[want] {
			t.Errorf("tool %q is missing", want)
		}
	}
}

func TestReadOnlyWithholdsMutatingTools(t *testing.T) {
	app := gctest.New(t, New(Config{ReadOnly: true}))

	list := rpc(t, app, "tools/list", nil)
	tools, _ := list["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		switch name {
		case "cancel_order", "mark_order_paid", "update_variant_inventory", "create_fulfillment":
			t.Errorf("read-only mode still offers the mutating tool %q", name)
		}
	}
}

// TestScriptedAgentFlow is the milestone's proof: an agent runs a store
// through the same domain services a person would use, and every change it
// makes is recorded.
func TestScriptedAgentFlow(t *testing.T) {
	app := gctest.New(t, New(Config{}))
	ctx := context.Background()

	// A shopper places an order the ordinary way.
	result := gctest.PlaceOrder(t, app, gocommerce.CodeCOD)

	// The agent orients itself.
	info, isErr := callTool(t, app, "store_info", nil)
	if isErr {
		t.Fatalf("store_info failed: %s", info)
	}
	if !strings.Contains(info, `"currency": "USD"`) {
		t.Errorf("store_info should report the currency: %s", info)
	}

	// It finds the order.
	orders, isErr := callTool(t, app, "list_orders", map[string]any{"status": "confirmed"})
	if isErr {
		t.Fatalf("list_orders failed: %s", orders)
	}
	if !strings.Contains(orders, result.Order.Number) {
		t.Errorf("list_orders should include %s: %s", result.Order.Number, orders)
	}

	// It settles, ships and delivers it.
	if out, isErr := callTool(t, app, "mark_order_paid",
		map[string]any{"order_id": result.Order.ID, "reference": "collected in cash"}); isErr {
		t.Fatalf("mark_order_paid failed: %s", out)
	}
	if out, isErr := callTool(t, app, "create_fulfillment",
		map[string]any{"order_id": result.Order.ID, "tracking": "AGENT-1"}); isErr {
		t.Fatalf("create_fulfillment failed: %s", out)
	}
	if out, isErr := callTool(t, app, "mark_order_delivered",
		map[string]any{"order_id": result.Order.ID}); isErr {
		t.Fatalf("mark_order_delivered failed: %s", out)
	}

	order, err := app.Order().Get(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != gocommerce.OrderDelivered || order.PaymentStatus != gocommerce.PaymentPaid {
		t.Errorf("order = (%s, %s), want (delivered, paid)", order.Status, order.PaymentStatus)
	}

	// It restocks what is running low.
	variant, err := app.Products().GetVariantBySKU(ctx, "GCTEST-cod")
	if err != nil {
		t.Fatalf("get variant: %v", err)
	}
	if out, isErr := callTool(t, app, "update_variant_inventory",
		map[string]any{"variant_id": variant.ID, "adjust": 40}); isErr {
		t.Fatalf("update_variant_inventory failed: %s", out)
	}
	restocked, err := app.Products().GetVariantBySKU(ctx, "GCTEST-cod")
	if err != nil {
		t.Fatalf("get variant: %v", err)
	}
	if restocked.StockOnHand != variant.StockOnHand+40 {
		t.Errorf("stock = %d, want %d", restocked.StockOnHand, variant.StockOnHand+40)
	}

	// Everything the agent changed is on the record. When software can cancel
	// orders on its own, "who did that" has to be answerable.
	var audited int
	if err := app.DB().QueryRowContext(ctx, `SELECT count(*) FROM mcp_audit`).Scan(&audited); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audited != 4 {
		t.Errorf("audit rows = %d, want 4 (one per mutating call)", audited)
	}

	rec := gctest.AdminRequest(t, app, http.MethodGet, "/api/admin/x/mcp/audit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit endpoint status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "mark_order_paid") {
		t.Errorf("audit log should name the tools called: %s", rec.Body)
	}
}

// TestToolErrorsReachTheAgent: a refused domain operation is an answer the
// agent can act on, not a transport failure.
func TestToolErrorsReachTheAgent(t *testing.T) {
	app := gctest.New(t, New(Config{}))

	result := gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
	if _, isErr := callTool(t, app, "create_fulfillment",
		map[string]any{"order_id": result.Order.ID, "tracking": "T-1"}); isErr {
		t.Fatal("shipping a confirmed order should succeed")
	}

	text, isErr := callTool(t, app, "cancel_order",
		map[string]any{"order_id": result.Order.ID, "reason": "agent changed its mind"})
	if !isErr {
		t.Fatal("cancelling a shipped order should be refused")
	}
	if !strings.Contains(text, "already shipped") {
		t.Errorf("the agent should be told why: %s", text)
	}

	// The refusal is audited too — an attempt is as interesting as a success.
	var outcome string
	if err := app.DB().QueryRowContext(context.Background(),
		`SELECT outcome FROM mcp_audit WHERE tool = 'cancel_order' ORDER BY id DESC LIMIT 1`).
		Scan(&outcome); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if outcome != "error" {
		t.Errorf("audited outcome = %q, want error", outcome)
	}
}

func TestUnknownMethodAndTool(t *testing.T) {
	app := gctest.New(t, New(Config{}))

	rec := gctest.AdminRequest(t, app, http.MethodPost, "/api/admin/x/mcp",
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "does/not/exist"})
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Errorf("error = %+v, want method-not-found", resp.Error)
	}
}
