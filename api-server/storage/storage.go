package storage

import (
	"context"
	"errors"

	"controlplane/protocol"
)

var (
	ErrNotFound        = errors.New("resource not found")
	ErrAlreadyExists   = errors.New("resource already exists")
	ErrKindNotFound    = errors.New("resource kind not found")
	ErrKindInUse       = errors.New("resource kind still has resources")
	ErrInvalidResource = errors.New("invalid resource")
)

type ResourceFilter struct {
	APIVersion string
	Kind       string
	Namespace  string
}

type OutboxEvent struct {
	ID    int64
	Event protocol.WatchEvent
}

type ResourceStore interface {
	Close() error

	Create(ctx context.Context, resource protocol.Resource) (protocol.Resource, error)

	Get(ctx context.Context, apiVersion, kind, namespace, name string) (protocol.Resource, error)

	List(ctx context.Context, filter ResourceFilter) ([]protocol.Resource, error)

	ListNamespaces(ctx context.Context) ([]string, error)

	Update(ctx context.Context, resource protocol.Resource) (protocol.Resource, error)

	Delete(ctx context.Context, apiVersion, kind, namespace, name string) (protocol.Resource, error)

	RegisterKind(ctx context.Context, kind protocol.ResourceKind) error

	DeleteKind(ctx context.Context, apiVersion, kind string) error

	ListKinds(ctx context.Context) ([]protocol.ResourceKind, error)

	ListPendingEvents(ctx context.Context, limit int) ([]OutboxEvent, error)

	MarkEventPublished(ctx context.Context, id int64) error
}
