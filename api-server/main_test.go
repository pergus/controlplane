package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"controlplane/api-server/storage"
	"controlplane/protocol"
)

func TestResourceValidationOnCreateAndUpdate(t *testing.T) {
	store, err := storage.NewSQLite(filepath.Join(t.TempDir(), "controlplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	server := httptest.NewServer(NewServer(store))
	defer server.Close()

	kind := protocol.ResourceKind{
		APIVersion: "v1",
		Kind:       "DNSRecord",
		Resource:   "dnsrecords",
		Namespaced: true,
		Schema: map[string]any{
			"type":     "object",
			"required": []string{"hostname", "address"},
			"properties": map[string]any{
				"hostname": map[string]any{"type": "string"},
				"address":  map[string]any{"type": "string"},
			},
			"additionalProperties": false,
		},
	}
	registerKind(t, server.URL, kind)

	invalidCreate := `{"metadata":{"name":"invalid"},"spec":{"hostname":"example.test"}}`
	response := request(t, http.MethodPost, server.URL+"/api/v1/DNSRecord", invalidCreate)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	response.Body.Close()

	validCreate := `{"metadata":{"name":"valid"},"spec":{"hostname":"example.test","address":"192.0.2.10"}}`
	response = request(t, http.MethodPost, server.URL+"/api/v1/DNSRecord", validCreate)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("valid create status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	response.Body.Close()

	invalidUpdate := `{"spec":{"hostname":"changed.test","unexpected":true}}`
	response = request(t, http.MethodPut, server.URL+"/api/v1/DNSRecord/valid", invalidUpdate)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	response.Body.Close()

	response = request(t, http.MethodGet, server.URL+"/api/v1/DNSRecord/valid", "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	var resource protocol.Resource
	if err := json.NewDecoder(response.Body).Decode(&resource); err != nil {
		t.Fatal(err)
	}
	if resource.Spec["hostname"] != "example.test" || resource.Spec["address"] != "192.0.2.10" {
		t.Fatalf("stored spec changed after rejected update: %#v", resource.Spec)
	}
}

func TestKindRegistrationRejectsInvalidSchema(t *testing.T) {
	store, err := storage.NewSQLite(filepath.Join(t.TempDir(), "controlplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	server := httptest.NewServer(NewServer(store))
	defer server.Close()

	body := `{"apiVersion":"v1","kind":"Broken","resource":"brokens","schema":{"type":"not-a-json-type"}}`
	response := request(t, http.MethodPost, server.URL+"/api/v1/kinds", body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid schema status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
}

func TestKindCRUD(t *testing.T) {
	store, err := storage.NewSQLite(filepath.Join(t.TempDir(), "controlplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	server := httptest.NewServer(NewServer(store))
	defer server.Close()

	kind := protocol.ResourceKind{
		APIVersion: "v1",
		Kind:       "Example",
		Resource:   "examples",
	}
	registerKind(t, server.URL, kind)
	kindURL := server.URL + "/api/v1/kinds/v1/Example"

	response := request(t, http.MethodGet, kindURL, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get kind status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	response.Body.Close()

	kind.Resource = "new-examples"
	updatedBody, err := json.Marshal(kind)
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, http.MethodPut, kindURL, string(updatedBody))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update kind status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	response.Body.Close()

	response = request(t, http.MethodPost, server.URL+"/api/v1/Example", `{"metadata":{"name":"sample"}}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create resource status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	response.Body.Close()

	response = request(t, http.MethodDelete, kindURL, "")
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("delete in-use kind status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	response.Body.Close()

	response = request(t, http.MethodDelete, server.URL+"/api/v1/Example/sample", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete resource status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	response.Body.Close()

	response = request(t, http.MethodDelete, kindURL, "")
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete kind status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	response.Body.Close()

	response = request(t, http.MethodGet, kindURL, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("get deleted kind status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	response.Body.Close()
}

func registerKind(t *testing.T, baseURL string, kind protocol.ResourceKind) {
	t.Helper()

	body, err := json.Marshal(kind)
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, http.MethodPost, baseURL+"/api/v1/kinds", string(body))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("kind registration status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
}

func request(t *testing.T, method, url, body string) *http.Response {
	t.Helper()

	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
