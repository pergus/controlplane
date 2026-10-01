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
