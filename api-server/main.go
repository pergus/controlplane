package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"controlplane/api-server/storage"
	"controlplane/messaging"
	"controlplane/protocol"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

const (
	defaultAddress    = ":8080"
	defaultSQLitePath = "controlplane.db"
)

type Watcher struct {
	id         uint64
	apiVersion string
	kind       string
	namespace  string

	events      chan protocol.WatchEvent
	done        chan struct{}
	requestDone <-chan struct{}
}

type Server struct {
	store  storage.ResourceStore
	broker *messaging.Client

	mu           sync.Mutex
	watchID      uint64
	watchers     map[uint64]*Watcher
	watchEnabled bool
}

var errWatchDisabled = errors.New("HTTP watch is disabled")

func NewServer(store storage.ResourceStore) *Server {
	return NewServerWithBroker(store, nil)
}

func NewServerWithBroker(store storage.ResourceStore, broker *messaging.Client) *Server {
	return &Server{
		store:        store,
		broker:       broker,
		watchers:     make(map[uint64]*Watcher),
		watchEnabled: true,
	}
}

func main() {
	store, err := createStore()
	if err != nil {
		log.Fatalf("failed to create storage: %v", err)
	}
	defer store.Close()

	broker, err := messaging.Connect(messaging.URLFromEnv(), "api-server")
	if err != nil {
		log.Fatalf("failed to connect to NATS: %v", err)
	}
	defer broker.Close()
	if err := broker.EnsureRegistrationStream(); err != nil {
		log.Fatalf("failed to ensure kind registration stream: %v", err)
	}
	kinds, err := store.ListKinds(context.Background())
	if err != nil {
		log.Fatalf("failed to list resource kinds: %v", err)
	}
	for _, kind := range kinds {
		if err := broker.EnsureKindEventStream(kind.APIVersion, kind.Kind); err != nil {
			log.Fatalf("failed to ensure event stream for %s/%s: %v", kind.APIVersion, kind.Kind, err)
		}
	}

	server := NewServerWithBroker(store, broker)
	workerContext, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	go server.runRegistrationConsumer(workerContext)
	go server.runOutboxPublisher(workerContext)

	httpServer := &http.Server{
		Addr:              getEnv("API_SERVER_ADDRESS", defaultAddress),
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)

	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("API server listening on %s", httpServer.Addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("API server failed: %v", err)
		}
	}()

	<-stop

	log.Println("shutting down API server")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

func createStore() (storage.ResourceStore, error) {
	backend := strings.ToLower(getEnv("STORAGE", "sqlite"))

	switch backend {
	case "sqlite":
		path := getEnv("SQLITE_PATH", defaultSQLitePath)

		log.Printf("using SQLite storage: %s", path)

		return storage.NewSQLite(path)

	case "postgres", "postgresql":
		dsn := os.Getenv("POSTGRES_DSN")

		if dsn == "" {
			return nil, fmt.Errorf("POSTGRES_DSN must be set when STORAGE=%s", backend)
		}

		log.Println("using PostgreSQL storage")

		return storage.NewPostgres(dsn)

	default:
		return nil, fmt.Errorf("unknown storage backend %q", backend)
	}
}

func getEnv(name, fallback string) string {
	value := os.Getenv(name)

	if value == "" {
		return fallback
	}

	return value
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")

	switch path {
	case "api/kinds":
		s.handleKinds(w, r)
		return

	case "api/resources":
		s.handleResources(w, r)
		return

	case "api/namespaces":
		s.handleNamespaces(w, r)
		return

	case "api/watch":
		s.handleWatch(w, r)
		return
	}

	if strings.HasPrefix(path, "api/kinds/") {
		parts := strings.Split(path, "/")
		if len(parts) == 4 {
			s.handleKind(w, r, parts[2], parts[3])
			return
		}
		http.NotFound(w, r)
		return
	}

	if !strings.HasPrefix(path, "api/") {
		http.NotFound(w, r)
		return
	}

	parts := strings.Split(path, "/")

	if len(parts) < 3 || parts[0] != "api" {
		http.NotFound(w, r)
		return
	}

	apiVersion := parts[1]
	kind := parts[2]

	if len(parts) == 3 {
		s.handleResourceCollection(w, r, apiVersion, kind)
		return
	}

	if len(parts) == 4 {
		s.handleResource(w, r, apiVersion, kind, parts[3])
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleKind(w http.ResponseWriter, r *http.Request, apiVersion, kindName string) {
	switch r.Method {
	case http.MethodGet:
		kinds, err := s.store.ListKinds(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, kind := range kinds {
			if kind.APIVersion == apiVersion && kind.Kind == kindName {
				writeJSON(w, http.StatusOK, kind)
				return
			}
		}
		writeStorageError(w, storage.ErrKindNotFound)

	case http.MethodPut:
		var kind protocol.ResourceKind
		if err := decodeJSON(r, &kind); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		if kind.APIVersion != apiVersion || kind.Kind != kindName {
			writeError(w, http.StatusBadRequest, errors.New("kind apiVersion/kind does not match URL"))
			return
		}
		if err := validateKind(kind); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.registerKind(r.Context(), kind); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, kind)

	case http.MethodDelete:
		if err := s.store.DeleteKind(r.Context(), apiVersion, kindName); err != nil {
			writeStorageError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func validateKind(kind protocol.ResourceKind) error {
	if kind.APIVersion == "" || kind.Kind == "" || kind.Resource == "" {
		return errors.New("apiVersion, kind, and resource are required")
	}
	if _, err := compileResourceSchema(kind.Schema); err != nil {
		return fmt.Errorf("invalid resource schema: %w", err)
	}
	return nil
}

func (s *Server) handleKinds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		kinds, err := s.store.ListKinds(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		result := protocol.ResourceKindList{
			APIVersion: "v1",
			Kind:       "KindList",
			Items:      kinds,
		}

		writeJSON(w, http.StatusOK, result)

	case http.MethodPost:
		var kind protocol.ResourceKind

		if err := decodeJSON(r, &kind); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}

		if err := validateKind(kind); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		if err := s.store.RegisterKind(r.Context(), kind); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.broker != nil {
			if err := s.broker.EnsureKindEventStream(kind.APIVersion, kind.Kind); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}

		writeJSON(w, http.StatusCreated, kind)

	default:
		w.Header().Set("Allow", "GET, POST")

		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func (s *Server) registerKind(ctx context.Context, kind protocol.ResourceKind) error {
	if err := validateKind(kind); err != nil {
		return err
	}
	if err := s.store.RegisterKind(ctx, kind); err != nil {
		return err
	}
	if s.broker != nil {
		return s.broker.EnsureKindEventStream(kind.APIVersion, kind.Kind)
	}
	return nil
}

func (s *Server) runRegistrationConsumer(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.broker.RunRegistrations(ctx, s.registerKind)
		if ctx.Err() != nil {
			return
		}
		log.Printf("kind registration consumer stopped: %v", err)
		if !waitForRetry(ctx) {
			return
		}
	}
}

func (s *Server) runOutboxPublisher(ctx context.Context) {
	for ctx.Err() == nil {
		events, err := s.store.ListPendingEvents(ctx, 100)
		shouldWait := len(events) == 0 || err != nil
		if err == nil {
			for _, event := range events {
				if err := s.broker.PublishEvent(ctx, event.Event); err != nil {
					log.Printf("failed to publish outbox event %d: %v", event.ID, err)
					shouldWait = true
					break
				}
				if err := s.store.MarkEventPublished(ctx, event.ID); err != nil {
					log.Printf("failed to mark outbox event %d published: %v", event.ID, err)
					shouldWait = true
					break
				}
			}
		} else {
			log.Printf("failed to read event outbox: %v", err)
		}
		if shouldWait {
			if !waitForRetry(ctx) {
				return
			}
		}
	}
}

func waitForRetry(ctx context.Context) bool {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		return
	}

	resources, err := s.store.List(r.Context(), storage.ResourceFilter{
		APIVersion: r.URL.Query().Get("apiVersion"),
		Kind:       r.URL.Query().Get("kind"),
		Namespace:  r.URL.Query().Get("namespace"),
	})

	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	result := map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceList",
		"items":      resources,
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		return
	}

	names, err := s.store.ListNamespaces(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	namespaces := make([]protocol.Namespace, 0, len(names))
	for _, name := range names {
		namespaces = append(namespaces, protocol.Namespace{Name: name})
	}
	writeJSON(w, http.StatusOK, protocol.NamespaceList{
		APIVersion: "v1",
		Kind:       "NamespaceList",
		Items:      namespaces,
	})
}

func (s *Server) handleResourceCollection(w http.ResponseWriter, r *http.Request, apiVersion, kind string) {
	switch r.Method {
	case http.MethodGet:
		s.listResources(w, r, apiVersion, kind)

	case http.MethodPost:
		s.createResource(w, r, apiVersion, kind)

	default:
		w.Header().Set("Allow", "GET, POST")

		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request, apiVersion, kind, name string) {
	switch r.Method {
	case http.MethodGet:
		s.getResource(w, r, apiVersion, kind, name)

	case http.MethodPut:
		s.updateResource(w, r, apiVersion, kind, name)

	case http.MethodDelete:
		s.deleteResource(w, r, apiVersion, kind, name)

	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")

		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func (s *Server) listResources(w http.ResponseWriter, r *http.Request, apiVersion, kind string) {
	resources, err := s.store.List(r.Context(), storage.ResourceFilter{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  r.URL.Query().Get("namespace"),
	})

	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	result := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind + "List",
		"items":      resources,
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createResource(w http.ResponseWriter, r *http.Request, apiVersion, kind string) {
	var resource protocol.Resource

	if err := decodeJSON(r, &resource); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	if resource.APIVersion == "" {
		resource.APIVersion = apiVersion
	}

	if resource.Kind == "" {
		resource.Kind = kind
	}

	if resource.APIVersion != apiVersion || resource.Kind != kind {
		writeError(w, http.StatusBadRequest, errors.New("resource apiVersion/kind does not match URL"))
		return
	}

	if resource.Metadata.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("metadata.name is required"))
		return
	}

	registeredKind, err := s.getKind(r.Context(), apiVersion, kind)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := validateResourceSpec(registeredKind.Schema, resource.Spec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	created, err := s.store.Create(r.Context(), resource)

	if err != nil {
		writeStorageError(w, err)
		return
	}

	s.broadcast(protocol.WatchEvent{
		Type:   protocol.Added,
		Object: created,
	})

	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request, apiVersion, kind, name string) {
	resource, err := s.store.Get(r.Context(), apiVersion, kind, r.URL.Query().Get("namespace"), name)

	if err != nil {
		writeStorageError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, resource)
}

func (s *Server) updateResource(w http.ResponseWriter, r *http.Request, apiVersion, kind, name string) {
	var resource protocol.Resource

	if err := decodeJSON(r, &resource); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}

	resource.APIVersion = apiVersion
	resource.Kind = kind
	resource.Metadata.Name = name

	registeredKind, err := s.getKind(r.Context(), apiVersion, kind)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	if err := validateResourceSpec(registeredKind.Schema, resource.Spec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	updated, err := s.store.Update(r.Context(), resource)

	if err != nil {
		writeStorageError(w, err)
		return
	}

	s.broadcast(protocol.WatchEvent{
		Type:   protocol.Modified,
		Object: updated,
	})

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteResource(w http.ResponseWriter, r *http.Request, apiVersion, kind, name string) {
	resource, err := s.store.Delete(r.Context(), apiVersion, kind, r.URL.Query().Get("namespace"), name)

	if err != nil {
		writeStorageError(w, err)
		return
	}

	s.broadcast(protocol.WatchEvent{
		Type:   protocol.Deleted,
		Object: resource,
	})

	writeJSON(w, http.StatusOK, resource)
}

func (s *Server) getKind(ctx context.Context, apiVersion, kind string) (protocol.ResourceKind, error) {
	kinds, err := s.store.ListKinds(ctx)
	if err != nil {
		return protocol.ResourceKind{}, err
	}

	for _, registered := range kinds {
		if registered.APIVersion == apiVersion && registered.Kind == kind {
			return registered, nil
		}
	}

	return protocol.ResourceKind{}, storage.ErrKindNotFound
}

func compileResourceSchema(definition map[string]any) (*jsonschema.Schema, error) {
	if len(definition) == 0 {
		return nil, nil
	}

	encoded, err := json.Marshal(definition)
	if err != nil {
		return nil, err
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("resource-schema.json", strings.NewReader(string(encoded))); err != nil {
		return nil, err
	}

	return compiler.Compile("resource-schema.json")
}

func validateResourceSpec(definition, spec map[string]any) error {
	schema, err := compileResourceSchema(definition)
	if err != nil {
		return fmt.Errorf("%w: invalid registered schema: %v", storage.ErrInvalidResource, err)
	}
	if schema == nil {
		return nil
	}
	if err := schema.Validate(spec); err != nil {
		return fmt.Errorf("%w: spec: %v", storage.ErrInvalidResource, err)
	}
	return nil
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
			return
		}
		if request.Enabled == nil {
			writeError(w, http.StatusBadRequest, errors.New("enabled is required"))
			return
		}
		s.setWatchEnabled(*request.Enabled)
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": *request.Enabled})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		return
	}

	apiVersion := r.URL.Query().Get("apiVersion")
	kind := r.URL.Query().Get("kind")
	namespace := r.URL.Query().Get("namespace")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	watcher, snapshot, err := s.addWatcher(r.Context(), apiVersion, kind, namespace)

	if err != nil {
		if errors.Is(err, errWatchDisabled) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	defer s.removeWatcher(watcher.id)

	encoder := json.NewEncoder(w)

	for _, resource := range snapshot {
		if err := encoder.Encode(protocol.WatchEvent{
			Type:   protocol.Added,
			Object: resource,
		}); err != nil {
			return
		}

		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case <-watcher.done:
			return

		case <-watcher.requestDone:
			return

		case event := <-watcher.events:
			if err := encoder.Encode(event); err != nil {
				return
			}

			flusher.Flush()
		}
	}
}

func (s *Server) addWatcher(
	ctx context.Context,
	apiVersion, kind, namespace string,
) (*Watcher, []protocol.Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.watchEnabled {
		return nil, nil, errWatchDisabled
	}

	s.watchID++

	watcher := &Watcher{
		id:          s.watchID,
		apiVersion:  apiVersion,
		kind:        kind,
		namespace:   namespace,
		events:      make(chan protocol.WatchEvent, 64),
		done:        make(chan struct{}),
		requestDone: ctx.Done(),
	}

	s.watchers[watcher.id] = watcher

	snapshot, err := s.store.List(ctx, storage.ResourceFilter{
		APIVersion: apiVersion,
		Kind:       kind,
		Namespace:  namespace,
	})

	if err != nil {
		delete(s.watchers, watcher.id)
		return nil, nil, err
	}

	return watcher, snapshot, nil
}

func (s *Server) setWatchEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchEnabled == enabled {
		return
	}
	s.watchEnabled = enabled
	if !enabled {
		for _, watcher := range s.watchers {
			close(watcher.done)
		}
	}
}

func (s *Server) removeWatcher(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.watchers, id)
}

func (s *Server) broadcast(event protocol.WatchEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, watcher := range s.watchers {
		if watcher.apiVersion != "" && watcher.apiVersion != event.Object.APIVersion {
			continue
		}

		if watcher.kind != "" && watcher.kind != event.Object.Kind {
			continue
		}

		if watcher.namespace != "" && watcher.namespace != event.Object.Metadata.Namespace {
			continue
		}

		select {
		case watcher.events <- event:
		default:
			log.Printf("watcher %d event buffer full", watcher.id)
		}
	}
}

func decodeJSON(r *http.Request, value any) error {
	decoder := json.NewDecoder(bufio.NewReader(r.Body))

	decoder.DisallowUnknownFields()

	return decoder.Decode(value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")

	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error(), status)
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, err)

	case errors.Is(err, storage.ErrAlreadyExists):
		writeError(w, http.StatusConflict, err)

	case errors.Is(err, storage.ErrKindNotFound):
		writeError(w, http.StatusNotFound, err)

	case errors.Is(err, storage.ErrKindInUse):
		writeError(w, http.StatusConflict, err)

	case errors.Is(err, storage.ErrInvalidResource):
		writeError(w, http.StatusBadRequest, err)

	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
