package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// CircuitDynamoAPI defines the DynamoDB operations needed for circuit status.
type CircuitDynamoAPI interface {
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// CircuitStateResponse represents circuit breaker state in the admin API response.
type CircuitStateResponse struct {
	InstanceType  string `json:"instance_type"`
	State         string `json:"state"`
	FailureCount  int    `json:"failure_count"`
	LastFailure   string `json:"last_failure,omitempty"`
	ResetAt       string `json:"reset_at,omitempty"`
}

// circuitRecord matches the DynamoDB structure.
type circuitRecord struct {
	InstanceType        string `dynamodbav:"instance_type"`
	State               string `dynamodbav:"state"`
	InterruptionCount   int    `dynamodbav:"interruption_count"`
	FirstInterruptionAt string `dynamodbav:"first_interruption_at"`
	LastInterruptionAt  string `dynamodbav:"last_interruption_at"`
	OpenedAt            string `dynamodbav:"opened_at"`
	AutoResetAt         string `dynamodbav:"auto_reset_at"`
}

// CircuitHandler provides HTTP endpoints for circuit breaker status.
type CircuitHandler struct {
	dynamo    CircuitDynamoAPI
	tableName string
	auth      *AuthMiddleware
	log       *logging.Logger
}

// NewCircuitHandler creates a new circuit handler.
func NewCircuitHandler(dynamoClient CircuitDynamoAPI, tableName string, auth *AuthMiddleware) *CircuitHandler {
	return &CircuitHandler{
		dynamo:    dynamoClient,
		tableName: tableName,
		auth:      auth,
		log:       logging.WithComponent(logging.LogTypeAdmin, "circuit"),
	}
}

// RegisterRoutes registers circuit API routes on the given mux.
func (h *CircuitHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/circuit", h.auth.WrapFunc(h.ListCircuitStates))
}

// ListCircuitStates handles GET /api/circuit.
func (h *CircuitHandler) ListCircuitStates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if h.tableName == "" {
		h.writeJSON(w, http.StatusOK, map[string]interface{}{
			"circuits": []CircuitStateResponse{},
			"message":  "Circuit breaker table not configured",
		})
		return
	}

	var allItems []map[string]types.AttributeValue
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		output, err := h.dynamo.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(h.tableName),
			ExclusiveStartKey: lastEvaluatedKey,
		})
		if err != nil {
			h.log.Error("failed to scan circuit table", slog.String(logging.KeyError, err.Error()))
			h.writeError(w, http.StatusInternalServerError, "Failed to list circuit states", err.Error())
			return
		}
		allItems = append(allItems, output.Items...)
		if output.LastEvaluatedKey == nil {
			break
		}
		lastEvaluatedKey = output.LastEvaluatedKey
	}

	var states []CircuitStateResponse
	for _, item := range allItems {
		var record circuitRecord
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			h.log.Warn("failed to unmarshal circuit record", slog.String(logging.KeyError, err.Error()))
			continue
		}

		resp := CircuitStateResponse{
			InstanceType: record.InstanceType,
			State:        record.State,
			FailureCount: record.InterruptionCount,
			LastFailure:  record.LastInterruptionAt,
			ResetAt:      record.AutoResetAt,
		}
		states = append(states, resp)
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"circuits": states,
	})
}

func (h *CircuitHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *CircuitHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
