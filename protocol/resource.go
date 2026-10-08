package protocol

type Resource struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   Metadata       `json:"metadata"`
	Spec       map[string]any `json:"spec,omitempty"`
	Status     map[string]any `json:"status,omitempty"`
}

type Metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Generation      uint64            `json:"generation,omitempty"`
	ResourceVersion uint64            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

type ResourceKind struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Resource   string         `json:"resource"`
	Namespaced bool           `json:"namespaced"`
	Schema     map[string]any `json:"schema,omitempty"`
}

type ResourceKindList struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Items      []ResourceKind `json:"items"`
}

type Namespace struct {
	Name string `json:"name"`
}

type NamespaceList struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Items      []Namespace `json:"items"`
}

type EventType string

const (
	Added    EventType = "ADDED"
	Modified EventType = "MODIFIED"
	Deleted  EventType = "DELETED"
)

type WatchEvent struct {
	Type   EventType `json:"type"`
	Object Resource  `json:"object"`
}
