package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"

	"controlplane/protocol"
)

type Watcher struct {
	id         uint64
	apiVersion string
	kind       string
	namespace  string
	events     chan protocol.WatchEvent
	done       <-chan struct{}
}

type Store struct {
	mu       sync.RWMutex
	revision uint64
	watchID  uint64
	items    map[string]protocol.Resource
	kinds    map[string]protocol.ResourceKind
	watchers map[uint64]*Watcher
}

func NewStore() *Store {
	return &Store{
		items:    make(map[string]protocol.Resource),
		kinds:    make(map[string]protocol.ResourceKind),
		watchers: make(map[uint64]*Watcher),
	}
}

func resourceKey(apiVersion, kind, namespace, name string) string {
	return apiVersion + "/" + kind + "/" + namespace + "/" + name
}

func kindKey(apiVersion, kind string) string {
	return apiVersion + "/" + kind
}

func (s *Store) RegisterKind(kind protocol.ResourceKind) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.kinds[kindKey(kind.APIVersion, kind.Kind)] = kind
}

func (s *Store) ListKinds() []protocol.ResourceKind {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]protocol.ResourceKind, 0, len(s.kinds))

	for _, kind := range s.kinds {
		result = append(result, kind)
	}

	return result
}

func (s *Store) Create(resource protocol.Resource) (protocol.Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := resourceKey(resource.APIVersion, resource.Kind, resource.Metadata.Namespace, resource.Metadata.Name)

	if _, exists := s.items[key]; exists {
		return protocol.Resource{}, fmt.Errorf("resource already exists")
	}

	resource.Metadata.UID = uuid.NewString()
	resource.Metadata.Generation = 1

	s.revision++
	resource.Metadata.ResourceVersion = s.revision

	s.items[key] = resource

	s.broadcastLocked(protocol.WatchEvent{
		Type:   protocol.Added,
		Object: resource,
	})

	return resource, nil
}

func (s *Store) Get(apiVersion, kind, namespace, name string) (protocol.Resource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resource, exists := s.items[resourceKey(apiVersion, kind, namespace, name)]
	return resource, exists
}

func (s *Store) List(apiVersion, kind, namespace string) []protocol.Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]protocol.Resource, 0)

	for _, resource := range s.items {
		if apiVersion != "" && resource.APIVersion != apiVersion {
			continue
		}

		if kind != "" && resource.Kind != kind {
			continue
		}

		if namespace != "" && resource.Metadata.Namespace != namespace {
			continue
		}

		result = append(result, resource)
	}

	return result
}

func (s *Store) Update(resource protocol.Resource) (protocol.Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := resourceKey(resource.APIVersion, resource.Kind, resource.Metadata.Namespace, resource.Metadata.Name)

	existing, exists := s.items[key]
	if !exists {
		return protocol.Resource{}, fmt.Errorf("resource not found")
	}

	resource.Metadata.UID = existing.Metadata.UID
	resource.Metadata.Generation = existing.Metadata.Generation

	if !jsonEqual(existing.Spec, resource.Spec) {
		resource.Metadata.Generation++
	}

	s.revision++
	resource.Metadata.ResourceVersion = s.revision

	s.items[key] = resource

	s.broadcastLocked(protocol.WatchEvent{
		Type:   protocol.Modified,
		Object: resource,
	})

	return resource, nil
}

func (s *Store) Delete(apiVersion, kind, namespace, name string) (protocol.Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := resourceKey(apiVersion, kind, namespace, name)

	resource, exists := s.items[key]
	if !exists {
		return protocol.Resource{}, fmt.Errorf("resource not found")
	}

	delete(s.items, key)

	s.revision++
	resource.Metadata.ResourceVersion = s.revision

	s.broadcastLocked(protocol.WatchEvent{
		Type:   protocol.Deleted,
		Object: resource,
	})

	return resource, nil
}

func (s *Store) Watch(apiVersion, kind, namespace string, done <-chan struct{}) *Watcher {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.watchID++

	watcher := &Watcher{
		id:         s.watchID,
		apiVersion: apiVersion,
		kind:       kind,
		namespace:  namespace,
		events:     make(chan protocol.WatchEvent, 64),
		done:       done,
	}

	s.watchers[watcher.id] = watcher

	return watcher
}

func (s *Store) RemoveWatcher(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.watchers, id)
}

func (s *Store) SnapshotForWatcher(watcher *Watcher) []protocol.WatchEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]protocol.WatchEvent, 0)

	for _, resource := range s.items {
		if watcher.apiVersion != "" && resource.APIVersion != watcher.apiVersion {
			continue
		}

		if watcher.kind != "" && resource.Kind != watcher.kind {
			continue
		}

		if watcher.namespace != "" && resource.Metadata.Namespace != watcher.namespace {
			continue
		}

		events = append(events, protocol.WatchEvent{
			Type:   protocol.Added,
			Object: resource,
		})
	}

	return events
}

func (s *Store) broadcastLocked(event protocol.WatchEvent) {
	for _, watcher := range s.watchers {
		if watcher.apiVersion != "" && event.Object.APIVersion != watcher.apiVersion {
			continue
		}

		if watcher.kind != "" && event.Object.Kind != watcher.kind {
			continue
		}

		if watcher.namespace != "" && event.Object.Metadata.Namespace != watcher.namespace {
			continue
		}

		select {
		case watcher.events <- event:
		case <-watcher.done:
			delete(s.watchers, watcher.id)
		}
	}
}

func jsonEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}

	right, err := json.Marshal(b)
	if err != nil {
		return false
	}

	return string(left) == string(right)
}

type Server struct {
	store *Store
}

func NewServer(store *Store) *Server {
	return &Server{
		store: store,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")

	if path == "" {
		http.NotFound(w, r)
		return
	}

	if path == "api/v1/kinds" {
		s.handleKinds(w, r)
		return
	}

	if path == "api/v1/resources" {
		s.handleResources(w, r)
		return
	}

	if path == "api/v1/watch" {
		s.handleWatch(w, r)
		return
	}

	if len(parts) < 3 || parts[0] != "api" {
		http.NotFound(w, r)
		return
	}

	apiVersion := parts[1]
	kind := parts[2]

	switch len(parts) {
	case 3:
		s.handleCollection(w, r, apiVersion, kind)
	case 4:
		s.handleResource(w, r, apiVersion, kind, parts[3])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleKinds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		result := protocol.ResourceKindList{
			APIVersion: "v1",
			Kind:       "KindList",
			Items:      s.store.ListKinds(),
		}

		writeJSON(w, http.StatusOK, result)

	case http.MethodPost:
		var kind protocol.ResourceKind

		if err := json.NewDecoder(r.Body).Decode(&kind); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		if kind.APIVersion == "" || kind.Kind == "" || kind.Resource == "" {
			http.Error(w, "apiVersion, kind, and resource are required", http.StatusBadRequest)
			return
		}

		s.store.RegisterKind(kind)

		writeJSON(w, http.StatusCreated, kind)

	default:
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		return
	}

	resources := s.store.List(r.URL.Query().Get("apiVersion"), r.URL.Query().Get("kind"), r.URL.Query().Get("namespace"))

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceList",
		"items":      resources,
	})
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	apiVersion := r.URL.Query().Get("apiVersion")
	kind := r.URL.Query().Get("kind")
	namespace := r.URL.Query().Get("namespace")

	watcher := s.store.Watch(apiVersion, kind, namespace, r.Context().Done())
	defer s.store.RemoveWatcher(watcher.id)

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	encoder := json.NewEncoder(w)

	for _, event := range s.store.SnapshotForWatcher(watcher) {
		if err := encoder.Encode(event); err != nil {
			return
		}

		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case event := <-watcher.events:
			if err := encoder.Encode(event); err != nil {
				return
			}

			flusher.Flush()
		}
	}
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request, apiVersion, kind string) {
	switch r.Method {
	case http.MethodGet:
		resources := s.store.List(apiVersion, kind, r.URL.Query().Get("namespace"))

		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind + "List",
			"items":      resources,
		})

	case http.MethodPost:
		var resource protocol.Resource

		if err := json.NewDecoder(r.Body).Decode(&resource); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		created, err := s.store.Create(resource)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}

		writeJSON(w, http.StatusCreated, created)

	default:
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request, apiVersion, kind, name string) {
	namespace := r.URL.Query().Get("namespace")

	switch r.Method {
	case http.MethodGet:
		resource, exists := s.store.Get(apiVersion, kind, namespace, name)
		if !exists {
			http.NotFound(w, r)
			return
		}

		writeJSON(w, http.StatusOK, resource)

	case http.MethodPut:
		var resource protocol.Resource

		if err := json.NewDecoder(r.Body).Decode(&resource); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		resource.APIVersion = apiVersion
		resource.Kind = kind
		resource.Metadata.Name = name

		updated, err := s.store.Update(resource)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		writeJSON(w, http.StatusOK, updated)

	case http.MethodDelete:
		deleted, err := s.store.Delete(apiVersion, kind, namespace, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		writeJSON(w, http.StatusOK, deleted)

	default:
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("failed to encode response: %v", err)
	}
}

func main() {
	store := NewStore()
	server := NewServer(store)

	log.Println("API server listening on :8080")

	if err := http.ListenAndServe(":8080", server); err != nil {
		log.Fatal(err)
	}
}
