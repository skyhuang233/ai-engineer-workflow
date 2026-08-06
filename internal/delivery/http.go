package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/skyhuang233/workflow/internal/plan"
	"github.com/skyhuang233/workflow/internal/store"
)

const controlPlaneTokenHeader = "X-Workflow-Control-Token"

const errorCodeHeader = "X-Workflow-Error-Code"

const (
	ErrorCodeNoActiveDeliveryPlan = "no_active_delivery_plan"
	ErrorCodeRetryableStore       = "retryable_store"
	ErrorCodeGatewayWritesPaused  = "gateway_writes_paused"
)

type HTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("control-plane gateway returned %d: %s", e.StatusCode, e.Message)
}

func (e *HTTPError) PollStoreFailure() bool {
	return e.Code == ErrorCodeRetryableStore
}

func (e *HTTPError) AuthenticationFailure() bool {
	return e.Code == ErrorCodeGatewayWritesPaused
}

type HTTPOptions struct {
	ControlPlaneToken string
}

type HTTPProjector struct {
	URL               string
	ControlPlaneToken string
	Client            *http.Client
}

func (p HTTPProjector) ProjectPlan(ctx context.Context, repository string, rootNumber int64, projection plan.Projection, label string) error {
	command := store.DeliveryRequest{Operation: store.DeliveryProjectPlan, Repository: repository, RootNumber: rootNumber, PlanProjection: &projection}
	if err := p.deliver(ctx, command); err != nil {
		return err
	}
	if label == "" {
		return nil
	}
	command.Operation = store.DeliveryAddIssueLabel
	command.Label = label
	return p.deliver(ctx, command)
}

func (p HTTPProjector) ProjectWorkflowInbox(ctx context.Context, repository string, questions []plan.WorkflowQuestion) error {
	return p.deliver(ctx, store.DeliveryRequest{Operation: store.DeliveryProjectInbox, Repository: repository, WorkflowQuestions: questions})
}

func (p HTTPProjector) deliver(ctx context.Context, command store.DeliveryRequest) error {
	if strings.TrimSpace(p.URL) == "" || strings.TrimSpace(p.ControlPlaneToken) == "" {
		return errors.New("control-plane gateway credentials are missing")
	}
	body, err := json.Marshal(command)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/v1/deliveries", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(controlPlaneTokenHeader, p.ControlPlaneToken)
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return &HTTPError{StatusCode: response.StatusCode, Code: response.Header.Get(errorCodeHeader), Message: strings.TrimSpace(string(message))}
	}
	return nil
}

func HTTPHandler(gateway Gateway, options ...HTTPOptions) http.Handler {
	option := HTTPOptions{}
	if len(options) > 0 {
		option = options[0]
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/deliveries" {
			http.NotFound(writer, request)
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
		decoder.DisallowUnknownFields()
		var command store.DeliveryRequest
		if err := decoder.Decode(&command); err != nil {
			http.Error(writer, "invalid delivery command", http.StatusBadRequest)
			return
		}
		if command.RunID == "" {
			if command.Operation != store.DeliveryProjectPlan && command.Operation != store.DeliveryProjectInbox && command.Operation != store.DeliveryAddIssueLabel {
				http.Error(writer, "unsupported control-plane delivery command", http.StatusForbidden)
				return
			}
			if option.ControlPlaneToken == "" || request.Header.Get(controlPlaneTokenHeader) != option.ControlPlaneToken {
				http.Error(writer, "control-plane authentication failed", http.StatusForbidden)
				return
			}
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			http.Error(writer, "invalid delivery command", http.StatusBadRequest)
			return
		}
		outbox, err := gateway.Submit(request.Context(), command)
		if err == nil {
			err = gateway.Dispatch(request.Context(), outbox.IdempotencyKey)
		}
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrDeliveryRejected) || errors.Is(err, store.ErrInvalidClaim) || errors.Is(err, store.ErrFencingConflict) {
				status = http.StatusConflict
			}
			if errors.Is(err, ErrGatewayWritesPaused) {
				status = http.StatusServiceUnavailable
				writer.Header().Set(errorCodeHeader, ErrorCodeGatewayWritesPaused)
			} else if errors.Is(err, store.ErrNoActiveDeliveryPlan) {
				writer.Header().Set(errorCodeHeader, ErrorCodeNoActiveDeliveryPlan)
			} else if errors.Is(err, ErrGatewayStore) || store.IsDatabaseError(err) {
				writer.Header().Set(errorCodeHeader, ErrorCodeRetryableStore)
			}
			http.Error(writer, err.Error(), status)
			return
		}
		outbox, err = gateway.Store.DeliveryOutbox(request.Context(), outbox.IdempotencyKey)
		if err != nil {
			writer.Header().Set(errorCodeHeader, ErrorCodeRetryableStore)
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(outbox)
	})
}
