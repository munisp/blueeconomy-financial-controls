package cvffapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
)

func createRequest(t *testing.T, idempotencyKey string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/cvff/applications", strings.NewReader(validCreateBody()))
	request.Header.Set("Authorization", testBearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	return request
}

func TestCreateApplicationStartsDisbursementWorkflow(t *testing.T) {
	store := &fakeStore{}
	starter := &fakeStarter{}
	handler := newTestHandlerWithStarter(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{}, starter)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, createRequest(t, "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", recorder.Code, recorder.Body)
	}
	if len(starter.started) != 1 || starter.started[0] != store.submitted.ApplicationID {
		t.Fatalf("starter calls = %v, want the new application ID", starter.started)
	}
}

func TestCreateApplicationStarterFailureIsFailClosed503(t *testing.T) {
	store := &fakeStore{}
	starter := &fakeStarter{err: errors.New("temporal frontend unreachable")}
	handler := newTestHandlerWithStarter(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{}, starter)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, createRequest(t, "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("starter failure = %d, want 503: %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("503 content type = %q", contentType)
	}
}

func TestCreateApplicationReplayReDrivesWorkflowStart(t *testing.T) {
	// A client retry with the same Idempotency-Key replays the intake and must
	// re-drive the workflow start: the starter is idempotent, so a submission
	// whose first start attempt failed heals on retry.
	store := &fakeStore{applications: []cvff.ApplicationDetail{seededApplication()}}
	starter := &fakeStarter{}
	handler := newTestHandlerWithStarter(t, store, &fakeBlobs{puts: map[string][]byte{}}, fakeScanner{}, starter)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, createRequest(t, "7f3a1c2e-9b0d-4e5f-a1b2-c3d4e5f60718"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("replay = %d: %s", recorder.Code, recorder.Body)
	}
	if len(starter.started) != 1 || starter.started[0] != "cvff-app-001" {
		t.Fatalf("starter calls = %v, want the retained application ID", starter.started)
	}
}

func TestNewTemporalStarterFailClosed(t *testing.T) {
	if _, err := NewTemporalStarter(nil, "cvff-task-queue"); err == nil {
		t.Fatal("nil temporal client accepted")
	}
}
