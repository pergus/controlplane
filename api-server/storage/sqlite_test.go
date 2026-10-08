package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"controlplane/protocol"
)

func TestResourceKindSchemaPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controlplane.db")
	kind := protocol.ResourceKind{
		APIVersion: "v1",
		Kind:       "Example",
		Resource:   "examples",
		Namespaced: true,
		Schema: map[string]any{
			"type":     "object",
			"required": []string{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		},
	}

	store, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterKind(context.Background(), kind); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	kinds, err := store.ListKinds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 {
		t.Fatalf("registered kind count = %d, want 1", len(kinds))
	}
	actualJSON, err := json.Marshal(kinds[0])
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(kind)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actualJSON, expectedJSON) {
		t.Fatalf("reloaded kind = %#v, want %#v", kinds[0], kind)
	}
}

func TestLegacyResourceKindsTableMigratesSchemaColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controlplane.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE resource_kinds (
			api_version TEXT NOT NULL,
			kind TEXT NOT NULL,
			resource TEXT NOT NULL,
			namespaced INTEGER NOT NULL,
			PRIMARY KEY(api_version, kind)
		)
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, err = db.Exec(`
		INSERT INTO resource_kinds(api_version, kind, resource, namespaced)
		VALUES ('v1', 'Legacy', 'legacies', 1)
	`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	kinds, err := store.ListKinds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 {
		t.Fatalf("kind count after migration = %d, want 1", len(kinds))
	}
	if kinds[0].Kind != "Legacy" || kinds[0].Resource != "legacies" || !kinds[0].Namespaced {
		t.Fatalf("legacy kind changed during migration: %#v", kinds[0])
	}
	if len(kinds[0].Schema) != 0 {
		t.Fatalf("legacy kind schema = %#v, want no schema", kinds[0].Schema)
	}
}

func TestResourceMutationsEnqueueOutboxEvents(t *testing.T) {
	store, err := NewSQLite(filepath.Join(t.TempDir(), "controlplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	resource := protocol.Resource{
		APIVersion: "v1",
		Kind:       "Example",
		Metadata: protocol.Metadata{
			Name:      "sample",
			Namespace: "default",
		},
		Spec: map[string]any{"value": "first"},
	}
	created, err := store.Create(ctx, resource)
	if err != nil {
		t.Fatal(err)
	}

	created.Spec["value"] = "second"
	if _, err := store.Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete(ctx, "v1", "Example", "default", "sample"); err != nil {
		t.Fatal(err)
	}

	events, err := store.ListPendingEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []protocol.EventType{protocol.Added, protocol.Modified, protocol.Deleted}
	if len(events) != len(wantTypes) {
		t.Fatalf("pending event count = %d, want %d", len(events), len(wantTypes))
	}
	for index, wantType := range wantTypes {
		if events[index].Event.Type != wantType {
			t.Errorf("event %d type = %q, want %q", index, events[index].Event.Type, wantType)
		}
		if events[index].Event.Object.Metadata.ResourceVersion == 0 {
			t.Errorf("event %d has no resourceVersion", index)
		}
	}

	if err := store.MarkEventPublished(ctx, events[0].ID); err != nil {
		t.Fatal(err)
	}
	events, err = store.ListPendingEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Event.Type != protocol.Modified {
		t.Fatalf("pending events after marking first = %#v, want modified and deleted", events)
	}
}

func TestDeleteKindRequiresNoResources(t *testing.T) {
	store, err := NewSQLite(filepath.Join(t.TempDir(), "controlplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	kind := protocol.ResourceKind{APIVersion: "v1", Kind: "Example", Resource: "examples"}
	if err := store.RegisterKind(ctx, kind); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, protocol.Resource{
		APIVersion: "v1",
		Kind:       "Example",
		Metadata:   protocol.Metadata{Name: "sample"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteKind(ctx, "v1", "Example"); err != ErrKindInUse {
		t.Fatalf("delete kind with resource error = %v, want %v", err, ErrKindInUse)
	}
	if _, err := store.Delete(ctx, "v1", "Example", "", "sample"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteKind(ctx, "v1", "Example"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteKind(ctx, "v1", "Example"); err != ErrKindNotFound {
		t.Fatalf("delete missing kind error = %v, want %v", err, ErrKindNotFound)
	}
}

func TestListNamespacesReturnsDistinctNonEmptyNames(t *testing.T) {
	store, err := NewSQLite(filepath.Join(t.TempDir(), "controlplane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	resources := []protocol.Resource{
		{APIVersion: "v1", Kind: "Example", Metadata: protocol.Metadata{Name: "one", Namespace: "default"}},
		{APIVersion: "v1", Kind: "Example", Metadata: protocol.Metadata{Name: "two", Namespace: "default"}},
		{APIVersion: "v1", Kind: "Example", Metadata: protocol.Metadata{Name: "system", Namespace: "kube-system"}},
		{APIVersion: "v1", Kind: "Example", Metadata: protocol.Metadata{Name: "cluster-scoped"}},
	}
	for _, resource := range resources {
		if _, err := store.Create(ctx, resource); err != nil {
			t.Fatal(err)
		}
	}

	namespaces, err := store.ListNamespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "kube-system"}
	if len(namespaces) != len(want) {
		t.Fatalf("namespaces = %#v, want %#v", namespaces, want)
	}
	for index := range want {
		if namespaces[index] != want[index] {
			t.Fatalf("namespaces = %#v, want %#v", namespaces, want)
		}
	}
}
