package delivery

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPHandlerRejectsUnauthenticatedControlPlaneCommands(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/deliveries", bytes.NewBufferString(`{"operation":"add_issue_label","repository":"owner/repo","root_number":10,"label":"arbitrary"}`))
	HTTPHandler(Gateway{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
