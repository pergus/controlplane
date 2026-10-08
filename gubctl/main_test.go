package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"controlplane/protocol"

	"github.com/spf13/cobra"
)

func TestReadManifestsSupportsResourcesAndKinds(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "definitions.yaml")
	content := `apiVersion: v1
kind: DNSRecord
metadata:
  name: example
  namespace: default
spec:
  hostname: example.test
  address: 192.0.2.10
---
apiVersion: v1
kind: Widget
resource: widgets
namespaced: true
schema:
  type: object
`
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	definitions, err := readManifests(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 {
		t.Fatalf("definition count = %d, want 2", len(definitions))
	}
	resource := definitions[0].resource
	if resource == nil || resource.Kind != "DNSRecord" || resource.Metadata.Name != "example" {
		t.Fatalf("first definition = %#v, want DNSRecord/example", definitions[0])
	}
	if resource.Spec["address"] != "192.0.2.10" {
		t.Fatalf("resource spec = %#v, want address", resource.Spec)
	}
	kind := definitions[1].kind
	if kind == nil || kind.Kind != "Widget" || kind.Resource != "widgets" || !kind.Namespaced {
		t.Fatalf("second definition = %#v, want Widget kind definition", definitions[1])
	}
}

func TestApplyKindCreatesThenUpdates(t *testing.T) {
	exists := false
	var postCount, putCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if exists {
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Widget","resource":"widgets","namespaced":true}`))
				return
			}
			http.NotFound(writer, request)
		case http.MethodPost:
			postCount++
			exists = true
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Widget","resource":"widgets","namespaced":true}`))
		case http.MethodPut:
			putCount++
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Widget","resource":"widgets","namespaced":true}`))
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL)
	definition := manifest{kind: &protocol.ResourceKind{APIVersion: "v1", Kind: "Widget", Resource: "widgets", Namespaced: true}}
	if _, err := client.applyDefinition(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if _, err := client.applyDefinition(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if postCount != 1 || putCount != 1 {
		t.Fatalf("POST count = %d, PUT count = %d, want one each", postCount, putCount)
	}
}

func TestApplyResourceCreatesUpdatesAndDeletes(t *testing.T) {
	exists := false
	var postCount, putCount, deleteCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if exists {
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"}}`))
				return
			}
			http.NotFound(writer, request)
		case http.MethodPost:
			postCount++
			exists = true
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"}}`))
		case http.MethodPut:
			putCount++
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"}}`))
		case http.MethodDelete:
			deleteCount++
			exists = false
			writer.WriteHeader(http.StatusOK)
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := newAPIClient(server.URL)
	definition := manifest{resource: &protocol.Resource{
		APIVersion: "v1",
		Kind:       "DNSRecord",
		Metadata:   protocol.Metadata{Name: "example", Namespace: "default"},
	}}
	if _, err := client.applyDefinition(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if _, err := client.applyDefinition(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if err := client.deleteDefinition(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if postCount != 1 || putCount != 1 || deleteCount != 1 {
		t.Fatalf("POST = %d, PUT = %d, DELETE = %d, want one each", postCount, putCount, deleteCount)
	}
}

func TestConsumeWatchFiltersKindNameNamespaceAndType(t *testing.T) {
	events := []string{
		`{"type":"ADDED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"one","namespace":"default"}}}`,
		`{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"two","namespace":"default"}}}`,
		`{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"two","namespace":"other"}}}`,
		`{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"Certificate","metadata":{"name":"two","namespace":"default"}}}`,
	}
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	err := consumeWatch(command, "table", strings.NewReader(strings.Join(events, "\n")), "DNSRecord", "two", "default", []string{"modified"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "MODIFIED") != 1 || !strings.Contains(output.String(), "DNSRecord") || !strings.Contains(output.String(), "two") {
		t.Fatalf("filtered watch output = %q", output.String())
	}
}

func TestFindKindRequiresVersionWhenAmbiguous(t *testing.T) {
	kinds := []protocol.ResourceKind{
		{APIVersion: "v1", Kind: "Widget", Resource: "widgets"},
		{APIVersion: "v2", Kind: "Widget", Resource: "widgets"},
	}
	if _, err := findKind(kinds, "Widget", ""); err == nil {
		t.Fatal("expected ambiguous API version error")
	}
	selected, err := findKind(kinds, "widgets", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if selected.APIVersion != "v2" {
		t.Fatalf("selected version = %q, want v2", selected.APIVersion)
	}
}

func TestGetAllResourcesUsesGlobalEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/resources" {
			http.NotFound(writer, request)
			return
		}
		if request.URL.Query().Get("namespace") != "default" {
			http.Error(writer, "namespace filter missing", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`{"items":[{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example","namespace":"default"}}]}`))
	}))
	defer server.Close()

	command := newRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--server", server.URL, "--namespace", "default", "get", "all"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "example") || !strings.Contains(output.String(), "DNSRecord") {
		t.Fatalf("get all output = %q", output.String())
	}
}

func TestGetNamespaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/namespaces" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"NamespaceList","items":[{"name":"default"},{"name":"kube-system"}]}`))
	}))
	defer server.Close()

	command := newRootCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--server", server.URL, "get", "namespaces"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "default") || !strings.Contains(output.String(), "kube-system") {
		t.Fatalf("namespace output = %q", output.String())
	}
}

func TestDescribeResourceAndKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/kinds":
			_, _ = writer.Write([]byte(`{"items":[{"apiVersion":"v1","kind":"DNSRecord","resource":"dnsrecords","namespaced":true},{"apiVersion":"v1","kind":"Widget","resource":"widgets","namespaced":true}]}`))
		case "/api/v1/DNSRecord/example":
			if request.URL.Query().Get("namespace") != "default" {
				http.Error(writer, "namespace missing", http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example","namespace":"default","uid":"abc123","generation":2,"resourceVersion":7,"labels":{"owner":"network"}},"spec":{"hostname":"example.test","address":"192.0.2.10"},"status":{"ready":true}}`))
		case "/api/kinds/v1/Widget":
			_, _ = writer.Write([]byte(`{"apiVersion":"v1","kind":"Widget","resource":"widgets","namespaced":true,"schema":{"type":"object"}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	resourceCommand := newRootCommand()
	var resourceOutput bytes.Buffer
	resourceCommand.SetOut(&resourceOutput)
	resourceCommand.SetArgs([]string{"--server", server.URL, "-n", "default", "describe", "DNSRecord/example"})
	if err := resourceCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Name:", "example", "DNSRecord", "default", "abc123", "example.test", "192.0.2.10", "ready"} {
		if !strings.Contains(resourceOutput.String(), expected) {
			t.Errorf("resource description missing %q:\n%s", expected, resourceOutput.String())
		}
	}

	kindCommand := newRootCommand()
	var kindOutput bytes.Buffer
	kindCommand.SetOut(&kindOutput)
	kindCommand.SetArgs([]string{"--server", server.URL, "describe", "kind", "Widget"})
	if err := kindCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Widget", "v1", "widgets", "Namespaced", "type: object"} {
		if !strings.Contains(kindOutput.String(), expected) {
			t.Errorf("kind description missing %q:\n%s", expected, kindOutput.String())
		}
	}
}
