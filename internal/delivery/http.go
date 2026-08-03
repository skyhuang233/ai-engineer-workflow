package delivery

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/skyhuang233/workflow/internal/store"
)

func HTTPHandler(gateway Gateway) http.Handler {
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
			http.Error(writer, "control-plane commands are not accepted over the agent gateway", http.StatusForbidden)
			return
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
			http.Error(writer, err.Error(), status)
			return
		}
		outbox, err = gateway.Store.DeliveryOutbox(request.Context(), outbox.IdempotencyKey)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(outbox)
	})
}
