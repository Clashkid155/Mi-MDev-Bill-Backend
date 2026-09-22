package reply

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestOK(t *testing.T) {
	rec := httptest.NewRecorder()
	OK(rec, 200, map[string]string{"id": "1"})
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d content-type=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "successful" || got.Error != "" {
		t.Fatalf("%+v", got)
	}
}

func TestFail(t *testing.T) {
	rec := httptest.NewRecorder()
	Fail(rec, 400, "bad request")
	var got Envelope
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != "failed" || got.Error != "bad request" || got.Data != nil {
		t.Fatalf("%+v", got)
	}
}

func TestOKWithMessageOmitsEmptyFields(t *testing.T) {
	rec := httptest.NewRecorder()
	OKWithMessage(rec, 200, "note", nil)
	body := rec.Body.String()
	if body != `{"status":"successful","message":"note"}`+"\n" {
		t.Fatalf("data/error must be omitted when empty, got %q", body)
	}
}
