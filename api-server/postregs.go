package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"controlplane/protocol"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type PostgresStore struct {
	db *sql.DB
}

func NewPostgres(dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}

	store := &PostgresStore{
		db: db,
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

func (s *PostgresStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value BIGINT NOT NULL
		);

		INSERT INTO metadata(key, value)
		VALUES ('resource_version', 0)
		ON CONFLICT(key) DO NOTHING;

		CREATE TABLE IF NOT EXISTS resources (
			api_version TEXT NOT NULL,
			kind TEXT NOT NULL,
			namespace TEXT NOT NULL,
			name TEXT NOT NULL,
			uid TEXT NOT NULL,
			generation BIGINT NOT NULL,
			resource_version BIGINT NOT NULL,
			labels JSONB,
			annotations JSONB,
			spec JSONB,
			status JSONB,
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
			namespaced BOOLEAN NOT NULL,
			PRIMARY KEY(api_version, kind)
		);
	`)

	return err
}

func (s *PostgresStore) Create(
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

	uid, err := generateUID()
	if err != nil {
		return protocol.Resource{}, err
	}

	resource.Metadata.UID = uid
	resource.Metadata.Generation = 1

	resource.Metadata.ResourceVersion, err =
		nextPostgresResourceVersion(ctx, tx)

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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`,
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
		resource.Metadata.UID,
		resource.Metadata.Generation,
		resource.Metadata.ResourceVersion,
		labels,
		annotations,
		spec,
		status,
	)

	if err != nil {
		if isUniqueViolation(err) {
			return protocol.Resource{}, ErrAlreadyExists
		}

		return protocol.Resource{}, err
	}

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *PostgresStore) Get(
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
		WHERE api_version = $1
		  AND kind = $2
		  AND namespace = $3
		  AND name = $4
		`,
		apiVersion,
		kind,
		namespace,
		name,
	)

	return scanPostgresResource(row)
}

func (s *PostgresStore) List(
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
	position := 1

	if filter.APIVersion != "" {
		query += " AND api_version = $" + fmt.Sprint(position)
		args = append(args, filter.APIVersion)
		position++
	}

	if filter.Kind != "" {
		query += " AND kind = $" + fmt.Sprint(position)
		args = append(args, filter.Kind)
		position++
	}

	if filter.Namespace != "" {
		query += " AND namespace = $" + fmt.Sprint(position)
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
		resource, err := scanPostgresResource(rows)
		if err != nil {
			return nil, err
		}

		resources = append(resources, resource)
	}

	return resources, rows.Err()
}

func (s *PostgresStore) Update(
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
		WHERE api_version = $1
		  AND kind = $2
		  AND namespace = $3
		  AND name = $4
		FOR UPDATE
		`,
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
	).Scan(&uid, &generation)

	if err != nil {
		if err == sql.ErrNoRows {
			return protocol.Resource{}, ErrNotFound
		}

		return protocol.Resource{}, err
	}

	generation++

	resource.Metadata.UID = uid
	resource.Metadata.Generation = generation

	resource.Metadata.ResourceVersion, err =
		nextPostgresResourceVersion(ctx, tx)

	if err != nil {
		return protocol.Resource{}, err
	}

	_, err = tx.ExecContext(
		ctx,
		`
		UPDATE resources
		SET
			generation = $1,
			resource_version = $2,
			labels = $3,
			annotations = $4,
			spec = $5,
			status = $6
		WHERE api_version = $7
		  AND kind = $8
		  AND namespace = $9
		  AND name = $10
		`,
		resource.Metadata.Generation,
		resource.Metadata.ResourceVersion,
		labels,
		annotations,
		spec,
		status,
		resource.APIVersion,
		resource.Kind,
		resource.Metadata.Namespace,
		resource.Metadata.Name,
	)

	if err != nil {
		return protocol.Resource{}, err
	}

	if err := tx.Commit(); err != nil {
		return protocol.Resource{}, err
	}

	return resource, nil
}

func (s *PostgresStore) Delete(
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

	resource, err := getPostgresTx(
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
		WHERE api_version = $1
		  AND kind = $2
		  AND namespace = $3
		  AND name = $4
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

func (s *PostgresStore) RegisterKind(
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
		VALUES ($1, $2, $3, $4)
		ON CONFLICT(api_version, kind)
		DO UPDATE SET
			resource = EXCLUDED.resource,
			namespaced = EXCLUDED.namespaced
		`,
		kind.APIVersion,
		kind.Kind,
		kind.Resource,
		kind.Namespaced,
	)

	return err
}

func (s *PostgresStore) ListKinds(
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

		if err := rows.Scan(
			&kind.APIVersion,
			&kind.Kind,
			&kind.Resource,
			&kind.Namespaced,
		); err != nil {
			return nil, err
		}

		kinds = append(kinds, kind)
	}

	return kinds, rows.Err()
}

func scanPostgresResource(s scanner) (protocol.Resource, error) {
	var (
		resource        protocol.Resource
		namespace       string
		labelsJSON      []byte
		annotationsJSON []byte
		specJSON        []byte
		statusJSON      []byte
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
		if err == sql.ErrNoRows {
			return protocol.Resource{}, ErrNotFound
		}

		return protocol.Resource{}, err
	}

	resource.Metadata.Namespace = namespace

	if len(labelsJSON) > 0 {
		if err := json.Unmarshal(labelsJSON, &resource.Metadata.Labels); err != nil {
			return protocol.Resource{}, err
		}
	}

	if len(annotationsJSON) > 0 {
		if err := json.Unmarshal(annotationsJSON, &resource.Metadata.Annotations); err != nil {
			return protocol.Resource{}, err
		}
	}

	if len(specJSON) > 0 {
		if err := json.Unmarshal(specJSON, &resource.Spec); err != nil {
			return protocol.Resource{}, err
		}
	}

	if len(statusJSON) > 0 {
		if err := json.Unmarshal(statusJSON, &resource.Status); err != nil {
			return protocol.Resource{}, err
		}
	}

	return resource, nil
}

func getPostgresTx(
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
		WHERE api_version = $1
		  AND kind = $2
		  AND namespace = $3
		  AND name = $4
		FOR UPDATE
		`,
		apiVersion,
		kind,
		namespace,
		name,
	)

	return scanPostgresResource(row)
}

func nextPostgresResourceVersion(
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
		FOR UPDATE
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
		SET value = $1
		WHERE key = 'resource_version'
		`,
		version,
	)

	return version, err
}

func generateUID() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError

	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}

	return false
}
