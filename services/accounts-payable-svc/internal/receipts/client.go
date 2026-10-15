// Package receipts is the client for AP-04 received-to-date -- contract #3:
//
//	GET /ap04/purchase-orders/{poID}/received-to-date
//	  keeps its current fields and ADDS "lines":[{"po_line_id","received_quantity","received_amount"}].
//
// If the endpoint or the per-line field is not there, the client reports
// ErrUnavailable / HasLines=false and the matcher records INCOMPLETE; missing
// receipt data is never a match.
package receipts

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"zoiko.io/accounts-payable-svc/internal/svcclient"
)

var ErrUnavailable = errors.New("goods-service-receipt-svc unavailable")

type Line struct {
	POLineID         string  `json:"po_line_id"`
	ReceivedQuantity float64 `json:"received_quantity"`
	ReceivedAmount   float64 `json:"received_amount"`
}

type ReceivedToDate struct {
	PurchaseOrderID string `json:"purchase_order_id"`
	Lines           []Line `json:"lines"`
	// HasLines is false when the response carried no per-line breakdown at all
	// (an old-shape response), which the matcher must treat as missing evidence.
	HasLines bool `json:"-"`
}

type Reader interface {
	ReceivedToDate(ctx context.Context, caller svcclient.Caller, purchaseOrderID string) (*ReceivedToDate, error)
}

type HTTPClient struct{ c *svcclient.Client }

func NewHTTPClient(baseURL string) *HTTPClient { return &HTTPClient{c: svcclient.New(baseURL)} }

func (h *HTTPClient) ReceivedToDate(ctx context.Context, caller svcclient.Caller, poID string) (*ReceivedToDate, error) {
	status, body, err := h.c.Do(ctx, caller, "GET", "/ap04/purchase-orders/"+url.PathEscape(poID)+"/received-to-date", nil)
	if err != nil || status != 200 {
		return nil, ErrUnavailable
	}
	var raw struct {
		PurchaseOrderID string  `json:"purchase_order_id"`
		Lines           *[]Line `json:"lines"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, ErrUnavailable
	}
	out := &ReceivedToDate{PurchaseOrderID: raw.PurchaseOrderID}
	if raw.Lines != nil {
		out.Lines = *raw.Lines
		out.HasLines = true
	}
	return out, nil
}
