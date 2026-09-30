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
			PRIMARY KEY(api_version, kind)
		);
	`)

	return err
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) Create(
	ctx context.Context,
	resource protocol.Resource,
) (protocol.Resource, error) {
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

	resource.Metadata.ResourceVersion, err =
		nextResourceVersion(ctx, tx)

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

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *SQLiteStore) Get(
	ctx context.Context,
	apiVersion string,
	kind string,
	namespace string,
	name string,
) (protocol.Resource, error) {
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

func (s *SQLiteStore) List(
	ctx context.Context,
	filter ResourceFilter,
) ([]protocol.Resource, error) {
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

func (s *SQLiteStore) Update(
	ctx context.Context,
	resource protocol.Resource,
) (protocol.Resource, error) {
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

	resource.Metadata.ResourceVersion, err =
		nextResourceVersion(ctx, tx)

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

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *SQLiteStore) Delete(
	ctx context.Context,
	apiVersion string,
	kind string,
	namespace string,
	name string,
) (protocol.Resource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Resource{}, err
	}
	defer tx.Rollback()

	resource, err := getSQLiteTx(
		ctx,
		tx,
		apiVersion,
		kind,
		namespace,
		name,
	)

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

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *SQLiteStore) RegisterKind(
	ctx context.Context,
	kind protocol.ResourceKind,
) error {
	_, err := s.db.ExecContext(
		ctx,
		`
		INSERT INTO resource_kinds (
			api_version,
			kind,
			resource,
			namespaced
		)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(api_version, kind)
		DO UPDATE SET
			resource = excluded.resource,
			namespaced = excluded.namespaced
		`,
		kind.APIVersion,
		kind.Kind,
		kind.Resource,
		boolToInt(kind.Namespaced),
	)

	return err
}

func (s *SQLiteStore) ListKinds(
	ctx context.Context,
) ([]protocol.ResourceKind, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`
		SELECT
			api_version,
			kind,
			resource,
			namespaced
		FROM resource_kinds
		ORDER BY api_version, kind
		`,
	)

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var kinds []protocol.ResourceKind

	for rows.Next() {
		var kind protocol.ResourceKind
		var namespaced int

		if err := rows.Scan(
			&kind.APIVersion,
			&kind.Kind,
			&kind.Resource,
			&namespaced,
		); err != nil {
			return nil, err
		}

		kind.Namespaced = namespaced != 0
		kinds = append(kinds, kind)
	}

	return kinds, rows.Err()
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

	err := s.Scan(
		&resource.APIVersion,
		&resource.Kind,
		&namespace,
		&resource.Metadata.Name,
		&resource.Metadata.UID,
		&resource.Metadata.Generation,
		&resource.Metadata.ResourceVersion,
		&labelsJSON,
		&annotationsJSON,
		&specJSON,
		&statusJSON,
	)

	if err != nil {
		if errorsIsNoRows(err) {
			return protocol.Resource{}, ErrNotFound
		}

		return protocol.Resource{}, err
	}

	resource.Metadata.Namespace = namespace

	if err := json.Unmarshal(
		[]byte(labelsJSON),
		&resource.Metadata.Labels,
	); err != nil {
		return protocol.Resource{}, err
	}

	if err := json.Unmarshal(
		[]byte(annotationsJSON),
		&resource.Metadata.Annotations,
	); err != nil {
		return protocol.Resource{}, err
	}

	if err := json.Unmarshal(
		[]byte(specJSON),
		&resource.Spec,
	); err != nil {
		return protocol.Resource{}, err
	}

	if err := json.Unmarshal(
		[]byte(statusJSON),
		&resource.Status,
	); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func getSQLiteTx(
	ctx context.Context,
	tx *sql.Tx,
	apiVersion string,
	kind string,
	namespace string,
	name string,
) (protocol.Resource, error) {
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

func nextResourceVersion(
	ctx context.Context,
	tx *sql.Tx,
) (uint64, error) {
	var version uint64

	err := tx.QueryRowContext(
		ctx,
		`
		SELECT value
		FROM metadata
		WHERE key = 'resource_version'
		`,
	).Scan(&version)

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
