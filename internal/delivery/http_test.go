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

func TestHTTPHandlerReportsReadinessOnlyToTheControlPlane(t *testing.T) {
	handler := HTTPHandler(Gateway{}, HTTPOptions{ControlPlaneToken: "control-secret"})

	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if unauthenticated.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated status = %d, body = %s", unauthenticated.Code, unauthenticated.Body.String())
	}

	authenticatedRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	authenticatedRequest.Header.Set(controlPlaneTokenHeader, "control-secret")
	authenticated := httptest.NewRecorder()
	handler.ServeHTTP(authenticated, authenticatedRequest)
	if authenticated.Code != http.StatusNoContent {
		t.Fatalf("authenticated status = %d, body = %s", authenticated.Code, authenticated.Body.String())
	}
}
