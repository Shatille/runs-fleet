package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type mockCircuitDynamoAPI struct {
	items []map[string]types.AttributeValue
	err   error
}

func (m *mockCircuitDynamoAPI) Scan(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &dynamodb.ScanOutput{Items: m.items}, nil
}

func TestCircuitHandler_ListCircuitStates(t *testing.T) {
	tests := []struct {
		name           string
		tableName      string
		items          []circuitRecord
		wantCount      int
		wantStatusCode int
	}{
		{
			name:      "list all circuit states",
			tableName: "circuit-table",
			items: []circuitRecord{
				{
					InstanceType:       "c7g.xlarge",
					State:              "open",
					InterruptionCount:  5,
					LastInterruptionAt: "2026-02-09T10:00:00Z",
					AutoResetAt:        "2026-02-09T10:30:00Z",
				},
				{
					InstanceType:      "m7g.large",
					State:             "closed",
					InterruptionCount: 0,
				},
			},
			wantCount:      2,
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "empty table",
			tableName:      "circuit-table",
			items:          []circuitRecord{},
			wantCount:      0,
			wantStatusCode: http.StatusOK,
		},
		{
			name:           "table not configured",
			tableName:      "",
			items:          []circuitRecord{},
			wantCount:      0,
			wantStatusCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var items []map[string]types.AttributeValue
			for _, record := range tt.items {
				item, _ := attributevalue.MarshalMap(record)
				items = append(items, item)
			}

			dynamoMock := &mockCircuitDynamoAPI{items: items}
			auth := NewAuthMiddleware("")
			handler := NewCircuitHandler(dynamoMock, tt.tableName, auth)

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/circuit", nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatusCode {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatusCode)
			}

			if tt.wantStatusCode == http.StatusOK {
				var resp struct {
					Circuits []CircuitStateResponse `json:"circuits"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				if len(resp.Circuits) != tt.wantCount {
					t.Errorf("got circuit count %d, want %d", len(resp.Circuits), tt.wantCount)
				}
			}
		})
	}
}

func TestCircuitHandler_OpenCircuit(t *testing.T) {
	record := circuitRecord{
		InstanceType:       "c7g.xlarge",
		State:              "open",
		InterruptionCount:  5,
		LastInterruptionAt: "2026-02-09T10:00:00Z",
		AutoResetAt:        "2026-02-09T10:30:00Z",
	}

	item, _ := attributevalue.MarshalMap(record)
	dynamoMock := &mockCircuitDynamoAPI{items: []map[string]types.AttributeValue{item}}
	auth := NewAuthMiddleware("")
	handler := NewCircuitHandler(dynamoMock, "circuit-table", auth)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/circuit", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", rec.Code, http.StatusOK)
	}

	var resp struct {
		Circuits []CircuitStateResponse `json:"circuits"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Circuits) != 1 {
		t.Fatalf("got circuit count %d, want 1", len(resp.Circuits))
	}

	circuit := resp.Circuits[0]
	if circuit.State != "open" {
		t.Errorf("got state %s, want open", circuit.State)
	}
	if circuit.FailureCount != 5 {
		t.Errorf("got failure count %d, want 5", circuit.FailureCount)
	}
	if circuit.ResetAt != "2026-02-09T10:30:00Z" {
		t.Errorf("got reset at %s, want 2026-02-09T10:30:00Z", circuit.ResetAt)
	}
}
