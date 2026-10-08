package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"time"

	"controlplane/protocol"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)

	store := &SQLiteStore{
		db: db,
	}

	if err := store.configure(); err != nil {
		db.Close()
		return nil, err
	}

	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *SQLiteStore) configure() error {
	_, err := s.db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		PRAGMA foreign_keys = ON;
	`)
	return err
}

func (s *SQLiteStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL
		);

		INSERT OR IGNORE INTO metadata(key, value)
		VALUES ('resource_version', 0);

		CREATE TABLE IF NOT EXISTS resources (
			api_version TEXT NOT NULL,
			kind TEXT NOT NULL,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			uid TEXT NOT NULL,
			generation INTEGER NOT NULL,
			resource_version INTEGER NOT NULL,
			labels TEXT,
			annotations TEXT,
			spec TEXT,
			status TEXT,
			PRIMARY KEY (
				api_version,
				kind,
				namespace,
				name
			)
		);

		CREATE INDEX IF NOT EXISTS idx_resources_kind
		ON resources(api_version, kind);

		CREATE INDEX IF NOT EXISTS idx_resources_namespace
		ON resources(namespace);

		CREATE TABLE IF NOT EXISTS resource_kinds (
			api_version TEXT NOT NULL,
			kind TEXT NOT NULL,
			resource TEXT NOT NULL,
			namespaced INTEGER NOT NULL,
			schema TEXT NOT NULL DEFAULT '{}',
			PRIMARY KEY(api_version, kind)
		);

		CREATE TABLE IF NOT EXISTS event_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			payload TEXT NOT NULL
		);
	`)

	if err != nil {
		return err
	}

	return s.ensureKindSchemaColumn()
}

func (s *SQLiteStore) ensureKindSchemaColumn() error {
	rows, err := s.db.Query("PRAGMA table_info(resource_kinds)")
	if err != nil {
		return err
	}

	hasSchema := false
	for rows.Next() {
		var (
			columnID     int
			name         string
			columnType   string
			notNull      int
			defaultValue sql.NullString
			primaryKey   int
		)

		if err := rows.Scan(&columnID, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}

		if name == "schema" {
			hasSchema = true
		}
	}

	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasSchema {
		return nil
	}

	_, err = s.db.Exec(`
		ALTER TABLE resource_kinds
		ADD COLUMN schema TEXT NOT NULL DEFAULT '{}'
	`)
	return err
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Create(ctx context.Context, resource protocol.Resource) (protocol.Resource, error) {
	labels, err := json.Marshal(resource.Metadata.Labels)
	if err != nil {
		return protocol.Resource{}, err
	}

	annotations, err := json.Marshal(resource.Metadata.Annotations)
	if err != nil {
		return protocol.Resource{}, err
	}

	spec, err := json.Marshal(resource.Spec)
	if err != nil {
		return protocol.Resource{}, err
	}

	status, err := json.Marshal(resource.Status)
	if err != nil {
		return protocol.Resource{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Resource{}, err
	}
	defer tx.Rollback()

	var exists int

	err = tx.QueryRowContext(
		ctx,
		`
		SELECT 1
		FROM resources
		WHERE api_version = ?
		  AND kind = ?
		  AND namespace = ?
		  AND name = ?
		`,
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
	).Scan(&exists)

	if err == nil {
		return protocol.Resource{}, ErrAlreadyExists
	}

	if !errorsIsNoRows(err) {
		return protocol.Resource{}, err
	}

	uid, err := newUID()
	if err != nil {
		return protocol.Resource{}, err
	}

	resource.Metadata.UID = uid
	resource.Metadata.Generation = 1

	resource.Metadata.ResourceVersion, err = nextResourceVersion(ctx, tx)

	if err != nil {
		return protocol.Resource{}, err
	}

	_, err = tx.ExecContext(
		ctx,
		`
		INSERT INTO resources (
			api_version,
			kind,
			namespace,
			name,
			uid,
			generation,
			resource_version,
			labels,
			annotations,
			spec,
			status
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
		resource.Metadata.UID,
		resource.Metadata.Generation,
		resource.Metadata.ResourceVersion,
		string(labels),
		string(annotations),
		string(spec),
		string(status),
	)

	if err != nil {
		return protocol.Resource{}, err
	}

	if err := enqueueSQLiteEvent(ctx, tx, protocol.WatchEvent{Type: protocol.Added, Object: resource}); err != nil {
		return protocol.Resource{}, err
	}

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *SQLiteStore) Get(ctx context.Context, apiVersion, kind, namespace, name string) (protocol.Resource, error) {
	row := s.db.QueryRowContext(
		ctx,
		`
		SELECT
			api_version,
			kind,
			namespace,
			name,
			uid,
			generation,
			resource_version,
			labels,
			annotations,
			spec,
			status
		FROM resources
		WHERE api_version = ?
		  AND kind = ?
		  AND namespace = ?
		  AND name = ?
		`,
		apiVersion,
		kind,
		namespace,
		name,
	)

	return scanResource(row)
}

func (s *SQLiteStore) List(ctx context.Context, filter ResourceFilter) ([]protocol.Resource, error) {
	query := `
		SELECT
			api_version,
			kind,
			namespace,
			name,
			uid,
			generation,
			resource_version,
			labels,
			annotations,
			spec,
			status
		FROM resources
		WHERE 1 = 1
	`

	var args []any

	if filter.APIVersion != "" {
		query += " AND api_version = ?"
		args = append(args, filter.APIVersion)
	}

	if filter.Kind != "" {
		query += " AND kind = ?"
		args = append(args, filter.Kind)
	}

	if filter.Namespace != "" {
		query += " AND namespace = ?"
		args = append(args, filter.Namespace)
	}

	query += `
		ORDER BY api_version, kind, namespace, name
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var resources []protocol.Resource

	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, err
		}

		resources = append(resources, resource)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return resources, nil
}

func (s *SQLiteStore) ListNamespaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT namespace
		FROM resources
		WHERE namespace <> ''
		ORDER BY namespace
		`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var namespaces []string
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			return nil, err
		}
		namespaces = append(namespaces, namespace)
	}
	return namespaces, rows.Err()
}

func (s *SQLiteStore) Update(ctx context.Context, resource protocol.Resource) (protocol.Resource, error) {
	spec, err := json.Marshal(resource.Spec)
	if err != nil {
		return protocol.Resource{}, err
	}

	status, err := json.Marshal(resource.Status)
	if err != nil {
		return protocol.Resource{}, err
	}

	labels, err := json.Marshal(resource.Metadata.Labels)
	if err != nil {
		return protocol.Resource{}, err
	}

	annotations, err := json.Marshal(resource.Metadata.Annotations)
	if err != nil {
		return protocol.Resource{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Resource{}, err
	}
	defer tx.Rollback()

	var (
		uid        string
		generation uint64
	)

	err = tx.QueryRowContext(
		ctx,
		`
		SELECT uid, generation
		FROM resources
		WHERE api_version = ?
		  AND kind = ?
		  AND namespace = ?
		  AND name = ?
		`,
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
	).Scan(&uid, &generation)

	if err != nil {
		if errorsIsNoRows(err) {
			return protocol.Resource{}, ErrNotFound
		}

		return protocol.Resource{}, err
	}

	generation++

	resource.Metadata.UID = uid
	resource.Metadata.Generation = generation

	resource.Metadata.ResourceVersion, err = nextResourceVersion(ctx, tx)

	if err != nil {
		return protocol.Resource{}, err
	}

	result, err := tx.ExecContext(
		ctx,
		`
		UPDATE resources
		SET
			generation = ?,
			resource_version = ?,
			labels = ?,
			annotations = ?,
			spec = ?,
			status = ?
		WHERE api_version = ?
		  AND kind = ?
		  AND namespace = ?
		  AND name = ?
		`,
		resource.Metadata.Generation,
		resource.Metadata.ResourceVersion,
		string(labels),
		string(annotations),
		string(spec),
		string(status),
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
	)

	if err != nil {
		return protocol.Resource{}, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return protocol.Resource{}, err
	}

	if affected != 1 {
		return protocol.Resource{}, ErrNotFound
	}

	if err := enqueueSQLiteEvent(ctx, tx, protocol.WatchEvent{Type: protocol.Modified, Object: resource}); err != nil {
		return protocol.Resource{}, err
	}

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, apiVersion, kind, namespace, name string) (protocol.Resource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Resource{}, err
	}
	defer tx.Rollback()

	resource, err := getSQLiteTx(ctx, tx, apiVersion, kind, namespace, name)

	if err != nil {
		return protocol.Resource{}, err
	}

	_, err = tx.ExecContext(
		ctx,
		`
		DELETE FROM resources
		WHERE api_version = ?
		  AND kind = ?
		  AND namespace = ?
		  AND name = ?
		`,
		apiVersion,
		kind,
		namespace,
		name,
	)

	if err != nil {
		return protocol.Resource{}, err
	}

	if err := enqueueSQLiteEvent(ctx, tx, protocol.WatchEvent{Type: protocol.Deleted, Object: resource}); err != nil {
		return protocol.Resource{}, err
	}

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *SQLiteStore) RegisterKind(ctx context.Context, kind protocol.ResourceKind) error {
	schemaJSON, err := json.Marshal(kind.Schema)
	if err != nil {
		return err
	}
	if kind.Schema == nil {
		schemaJSON = []byte("{}")
	}

	_, err = s.db.ExecContext(
		ctx,
		`
		INSERT INTO resource_kinds (
			api_version,
			kind,
			resource,
			namespaced,
			schema
		)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(api_version, kind)
		DO UPDATE SET
			resource = excluded.resource,
			namespaced = excluded.namespaced,
			schema = excluded.schema
		`,
		kind.APIVersion,
		kind.Kind,
		kind.Resource,
		boolToInt(kind.Namespaced),
		string(schemaJSON),
	)

	return err
}

func (s *SQLiteStore) DeleteKind(ctx context.Context, apiVersion, kind string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var resourceCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM resources WHERE api_version = ? AND kind = ?
		`, apiVersion, kind).Scan(&resourceCount); err != nil {
		return err
	}
	if resourceCount > 0 {
		return ErrKindInUse
	}

	result, err := tx.ExecContext(ctx, `
		DELETE FROM resource_kinds WHERE api_version = ? AND kind = ?
		`, apiVersion, kind)
	if err != nil {
		return err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrKindNotFound
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListKinds(ctx context.Context) ([]protocol.ResourceKind, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			api_version,
			kind,
			resource,
			namespaced,
			schema
		FROM resource_kinds
		ORDER BY api_version, kind
		`)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var kinds []protocol.ResourceKind

	for rows.Next() {
		var kind protocol.ResourceKind
		var namespaced int
		var schemaJSON string

		if err := rows.Scan(&kind.APIVersion, &kind.Kind, &kind.Resource, &namespaced, &schemaJSON); err != nil {
			return nil, err
		}

		kind.Namespaced = namespaced != 0
		if err := json.Unmarshal([]byte(schemaJSON), &kind.Schema); err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}

	return kinds, rows.Err()
}

func (s *SQLiteStore) ListPendingEvents(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, payload
		FROM event_outbox
		ORDER BY id
		LIMIT ?
		`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OutboxEvent
	for rows.Next() {
		var event OutboxEvent
		var payload string
		if err := rows.Scan(&event.ID, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &event.Event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *SQLiteStore) MarkEventPublished(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM event_outbox WHERE id = ?", id)
	return err
}

func enqueueSQLiteEvent(ctx context.Context, tx *sql.Tx, event protocol.WatchEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO event_outbox(payload) VALUES (?)", string(payload))
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanResource(s scanner) (protocol.Resource, error) {
	var (
		resource        protocol.Resource
		namespace       string
		labelsJSON      string
		annotationsJSON string
		specJSON        string
		statusJSON      string
	)

	err := s.Scan(&resource.APIVersion, &resource.Kind, &namespace, &resource.Metadata.Name, &resource.Metadata.UID, &resource.Metadata.Generation, &resource.Metadata.ResourceVersion, &labelsJSON, &annotationsJSON, &specJSON, &statusJSON)

	if err != nil {
		if errorsIsNoRows(err) {
			return protocol.Resource{}, ErrNotFound
		}

		return protocol.Resource{}, err
	}

	resource.Metadata.Namespace = namespace

	if err := json.Unmarshal([]byte(labelsJSON), &resource.Metadata.Labels); err != nil {
		return protocol.Resource{}, err
	}

	if err := json.Unmarshal([]byte(annotationsJSON), &resource.Metadata.Annotations); err != nil {
		return protocol.Resource{}, err
	}

	if err := json.Unmarshal([]byte(specJSON), &resource.Spec); err != nil {
		return protocol.Resource{}, err
	}

	if err := json.Unmarshal([]byte(statusJSON), &resource.Status); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func getSQLiteTx(ctx context.Context, tx *sql.Tx, apiVersion, kind, namespace, name string) (protocol.Resource, error) {
	row := tx.QueryRowContext(
		ctx,
		`
		SELECT
			api_version,
			kind,
			namespace,
			name,
			uid,
			generation,
			resource_version,
			labels,
			annotations,
			spec,
			status
		FROM resources
		WHERE api_version = ?
		  AND kind = ?
		  AND namespace = ?
		  AND name = ?
		`,
		apiVersion,
		kind,
		namespace,
		name,
	)

	return scanResource(row)
}

func nextResourceVersion(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var version uint64

	err := tx.QueryRowContext(ctx, `
		SELECT value
		FROM metadata
		WHERE key = 'resource_version'
		`).Scan(&version)

	if err != nil {
		return 0, err
	}

	version++

	_, err = tx.ExecContext(
		ctx,
		`
		UPDATE metadata
		SET value = ?
		WHERE key = 'resource_version'
		`,
		version,
	)

	if err != nil {
		return 0, err
	}

	return version, nil
}

func newUID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

func unixNano() int64 {
	return timeNowUnixNano()
}

func timeNowUnixNano() int64 {
	return time.Now().UnixNano()
}

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}
