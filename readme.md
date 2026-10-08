# Controlplane

Controlplane or Gubernator is a resource-based control plane written in Go. The API server stores resources and kind definitions, validates resource specifications, and provides a REST API and an HTTP watch endpoint. It also publishes resource events to NATS JetStream. Controllers register kinds through JetStream and consume durable per-kind event streams to reconcile external systems. The `gubctl` command-line client manages resources and kinds through the REST API.

---

# System Overview

The system has four main parts: the API server, NATS JetStream, API clients, and controllers.

```
                      +----------------------+
                      |      API Server      |
                      |                      |
                      | Resource Store       |
                      | Kind Registry        |
                      | REST API             |
                      | Watch API            |
                      +----------+-----------+
                                 |
                    +------------+------------+
                    |                         |
                    | REST API                | NATS JetStream
                    |                         |
           +--------v--------+       +--------v--------+
           | API clients,    |       | DNS Controller  |
           | gubctl, HTTP    |       | Certificate     |
           +-----------------+       | Controller      |
                                     +--------+--------+
                                              |
                                    +---------+---------+
                                    |                   |
                                    v                   v
                                  DNS system         PKI / ACME
```

The API server stores desired state and remains independent of controller-specific logic. Controllers register the kinds they manage, consume their event streams, and reconcile external systems. They do not call one another directly.

---

# Project Structure

The project has this structure:

```
controlplane/
├── Taskfile.yml
├── go.mod
│
├── protocol/
│   └── resource.go
│
├── nats-server.conf
│
├── messaging/
│   ├── client.go
│   └── streams.go
│
├── api-server/
│   ├── main.go
│   └── storage/
│       ├── storage.go
│       ├── sqlite.go
│       └── postregs.go
│
├── gubctl/
│   └── main.go
│
├── dns-controller/
│   └── main.go
│
└── certificate-controller/
    └── main.go
```

`protocol` defines shared resource and event types, while `messaging` provides NATS JetStream operations. `api-server` serves the REST API and persists state. `gubctl` provides the command-line client, and each controller reconciles its resource kinds with an external system.

## `protocol`

The `protocol` package contains the data structures shared by the API server and controllers. For example:

```go
package protocol

type Resource struct {
  APIVersion string         `json:"apiVersion"`
  Kind       string         `json:"kind"`
  Metadata   Metadata       `json:"metadata"`
  Spec       map[string]any `json:"spec,omitempty"`
  Status     map[string]any `json:"status,omitempty"`
}
```

Keep the protocol package small and independent of DNS, certificates, and controller logic.

The dependency direction is:

```
protocol
   ^
   |
   +-- api-server
   |
   +-- dns-controller
   |
   +-- certificate-controller
```

This dependency direction lets the API server and controllers use the same protocol without depending on each other's implementation.

---

# Resources

A resource represents desired state. It has five main parts:

```
apiVersion
kind
metadata
spec
status
```

Example:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "metadata": {
    "name": "example",
    "namespace": "default"
  },
  "spec": {
    "hostname": "example.test",
    "address": "192.168.1.10"
  }
}
```

## `apiVersion`

`apiVersion` identifies the resource API version. Use it to evolve a resource definition.

Example:

```json
"apiVersion": "v1"
```

Use `kind`, not `apiVersion`, to identify which controller manages a resource.

---

## `kind`

`kind` identifies the resource type and determines which controller manages it.

Examples:

```json
"kind": "DNSRecord"
```

and:

```json
"kind": "Certificate"
```

A controller normally manages one or more kinds. For example:

```
DNSRecord
    |
    +-- DNS controller

Certificate
    |
    +-- Certificate controller
```

---

## `metadata`

Metadata identifies a resource by name and can also include its namespace, UID, generation, resource version, labels, and annotations.

Example:

```json
"metadata": {
  "name": "example",
  "namespace": "default"
}
```

The complete metadata structure is:

The `resource_kinds` table stores registered kind definitions and their optional `spec` schemas. For example:
    ResourceVersion uint64            `json:"resourceVersion,omitempty"`
    Labels          map[string]string `json:"labels,omitempty"`
    Annotations     map[string]string `json:"annotations,omitempty"`
}
```

### `name`

The resource name identifies an object within its API version, kind, and namespace. For example:

```json
"name": "example"
```

### `namespace`

The namespace is optional. For example:

```json
"namespace": "default"
```

A resource is identified by:

```
apiVersion
kind
namespace
name
```

### `uid`

The API server creates a resource UID. A controller must not normally create it. For example:

```json
"uid": "7f3f0a7e-..."
```

### `generation`

The generation identifies changes to the desired resource configuration. It starts at `1` and increases when `spec` changes. A controller can use it to determine which configuration it has processed.

### `resourceVersion`

The API server manages `resourceVersion` and changes it when a resource changes. Controllers can use it to observe stored changes. The API may use it for concurrency control in the future.

### `labels`

Labels are key/value metadata. For example:

```json
"labels": {
  "environment": "production",
  "owner": "network"
}
```

### `annotations`

Annotations are key/value metadata for additional information. For example:

```json
"annotations": {
  "description": "Main DNS record"
}
```


---

# `spec`

`spec` contains the desired state. The API server validates it against the JSON Schema registered for the resource kind, but it does not apply controller-specific meaning or reconcile the requested state.

For example:

```json
"spec": {
  "hostname": "example.test",
  "address": "192.168.1.10"
}
```

The API server checks that the data matches the kind's schema, then stores it. The owning controller interprets the fields and reconciles the external system. This keeps the API server generic and gives each controller ownership of its resource's meaning.

For a DNS controller:

```
spec.hostname
spec.address
```

define a DNS record.

For a certificate controller:

```
spec.hostname
spec.issuer
```

define a certificate request.

The API server validates the schema but does not need to know what the values mean to a controller.

---

# `status`

`status` represents observed state.

Example:

```json
"status": {
  "ready": true
}
```

The example controllers log reconciliation activity but do not update resource status. A controller can use `status` to report information such as:

```
ready
error
message
observedGeneration
certificate expiry
DNS propagation state
```

The important distinction is:

```
spec   = desired state

status = observed state
```

For example:

```json
{
  "spec": {
    "hostname": "example.test",
    "address": "192.168.1.10"
  },
  "status": {
    "ready": true
  }
}
```

This means:

> The user wants `example.test` to point to `192.168.1.10`, and the controller reports that the requested state is currently ready.

---

# Resource Kinds

The API server supports dynamic resource-kind registration.

A resource kind is described by:

The `watchers` map contains active HTTP watch connections. They exist only in API-server memory and close when the server stops. When a resource changes, the server sends the event to matching watchers and records it in the transactional outbox for JetStream delivery.

`schema` is an optional JSON Schema for the resource's `spec`. The API server compiles the schema when the kind is registered, then validates every create and update before saving the resource or publishing a watch event. If no schema is registered, the API server performs no schema-based validation of `spec`.

For example, a DNS record kind can require a hostname and address:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true,
  "schema": {
    "type": "object",
    "required": ["hostname", "address"],
    "properties": {
      "hostname": { "type": "string", "minLength": 1 },
      "address": { "type": "string", "minLength": 1 }
    },
    "additionalProperties": false
  }
}
```

The schema validates structure and basic constraints. Controllers still own domain-specific checks, external-system constraints, and reconciliation behavior.

Example:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true
}
```

Another example:

```json
{
  "apiVersion": "v1",
  "kind": "Certificate",
  "resource": "certificates",
  "namespaced": true
}
```

The controller registers its resource kind through the JetStream registration stream when it starts. The API server creates a separate event stream for the registered kind.

The API server does not need a hard-coded list of resource kinds. For example, it does not need logic such as:

```go
if kind == "DNSRecord" {
    ...
}

if kind == "Certificate" {
    ...
}
```

Keep controller-specific logic in controllers. Each controller registers the resource kinds that it manages.

---

# API Server

The API server is the central process. It listens on:

```
http://localhost:8080
```

The API server provides resource-kind discovery, resource CRUD, resource listing, and HTTP resource watching. CRUD means create, read, update, and delete.

The API server stores resources and kind registrations through `ResourceStore`. SQLite is the default backend, and PostgreSQL is also supported. Each resource change and its outbox event are written in one transaction, then the event is published to JetStream. The HTTP watch endpoint still sends a snapshot followed by live NDJSON events, but its active connections remain in API-server memory.

Set `NATS_URL` to configure the broker. It defaults to `nats://localhost:4222`. Start NATS with JetStream before the API server or controllers. Controllers publish kind registrations to one stream. The API server validates and stores each kind, creates its event stream, then acknowledges the registration. Per-kind streams retain events for seven days, and durable consumers provide at-least-once delivery.

---

# API Server Store

The API server uses `ResourceStore` for resources, kinds, and pending outbox events. The database persists those records; the `Server` structure keeps active HTTP watchers in memory:

```go
type Server struct {
  store    storage.ResourceStore
  mu       sync.Mutex
  watchID  uint64
  watchers map[uint64]*Watcher
}
```

## Resources

Resources are stored by `apiVersion`, `kind`, `namespace`, and `name`. Different kinds or namespaces can therefore use the same name.

---

## Kinds

The `resource_kinds` table stores registered kind definitions and their optional `spec` schemas.

For example:

```
v1/DNSRecord
v1/Certificate
```

---

## Watchers

The `watchers` map contains active HTTP watch connections. They exist only in API-server memory and close when the server stops. When a resource changes, the API server sends the event to matching watchers and records it in the outbox for JetStream delivery.

---

# Concurrency

Go's HTTP server handles requests concurrently. The server mutex protects the in-memory watcher map, while the storage backend manages resource transactions:

```go
sync.Mutex
```

The mutex protects watcher registration and removal. Resource CRUD uses `ResourceStore`.

For example:

```
watch registration/removal
  |
  +-- mutex

resource create/update/delete
  |
  +-- ResourceStore
```

Keep watcher-map access synchronized. The storage implementation manages transaction safety for resource operations.

---

# API Endpoint Summary

The current API endpoints are:

| Method           | Endpoint                              | Purpose                            |
| -----------------| --------------------------------------| -----------------------------------|
| GET              | `/api/kinds`                          | List registered kinds              |
| POST             | `/api/kinds`                          | Register a kind                    |
| GET              | `/api/kinds/{apiVersion}/{kind}`      | Get a kind                         |
| PUT              | `/api/kinds/{apiVersion}/{kind}`      | Update a kind                      |
| DELETE           | `/api/kinds/{apiVersion}/{kind}`      | Delete an unused kind              |
| GET              | `/api/resources`                      | List resources                     |
| GET              | `/api/namespaces`                     | List resource namespaces           |
| GET, PUT         | `/api/watch`                          | Watch resources or set watch state |
| GET, POST        | `/api/{apiVersion}/{kind}`            | List or create resources           |
| GET, PUT, DELETE | `/api/{apiVersion}/{kind}/{name}`     | Get, update, or delete resource    |

Run `gubctl api-resources` to list these endpoints along with registered kinds.

`GET /api/namespaces` returns sorted, distinct, non-empty namespace names used by resources. Resources with an empty namespace are cluster-scoped and do not appear in this list.

Use `gubctl get namespaces` to list the namespaces in a table. Use `-o json` or `-o yaml` to select another format.

Send `PUT /api/watch` with `{"enabled": false}` to disable all HTTP watches. The server closes active watch streams and returns HTTP `503 Service Unavailable` to new watch requests. Send `{"enabled": true}` to allow new watches. The setting is held in API-server memory and resets to enabled after a restart. This control affects the HTTP watch endpoint only; JetStream controller delivery continues.

---

# List Resource Kinds

Endpoint:

```
GET /api/kinds
```

This endpoint returns all registered resource kinds.

Example:

```bash
curl http://localhost:8080/api/kinds
```

Example response:

```json
{
  "apiVersion": "v1",
  "kind": "KindList",
  "items": [
    {
      "apiVersion": "v1",
      "kind": "DNSRecord",
      "resource": "dnsrecords",
      "namespaced": true
    },
    {
      "apiVersion": "v1",
      "kind": "Certificate",
      "resource": "certificates",
      "namespaced": true
    }
  ]
}
```

The response has `apiVersion`, `kind`, and `items` fields. Each item contains one resource-kind definition.

# Register a Resource Kind

Endpoint:

```
POST /api/kinds
```

The request body must contain these fields:

```
apiVersion
kind
resource
```

`namespaced` is optional and defaults to `false`. `schema` is also optional; when present, it defines a JSON Schema for the resource's `spec`.

Example:

```bash
curl -X POST \-H 'Content-Type: application/json http://localhost:8080/api/kinds \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "resource": "dnsrecords",
    "namespaced": true,
    "schema": {
      "type": "object",
      "required": ["hostname", "address"],
      "properties": {
        "hostname": { "type": "string", "minLength": 1 },
        "address": { "type": "string", "minLength": 1 }
      },
      "additionalProperties": false
    }
  }'
```

The server validates the schema, stores the kind definition, and returns HTTP `201 Created` with the saved definition.

Example response:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true,
  "schema": {
    "type": "object",
    "required": ["hostname", "address"],
    "properties": {
      "hostname": { "type": "string", "minLength": 1 },
      "address": { "type": "string", "minLength": 1 }
    },
    "additionalProperties": false
  }
}
```

The kind and schema are stored in the selected database. A controller can register its kind again at startup; registration updates the existing definition. An invalid schema returns HTTP `400 Bad Request`. A resource create or update that fails schema validation also returns `400` and does not change stored state or publish an event.

---

# List All Resources

Endpoint:

```
GET /api/resources
```

Example:

```bash
curl http://localhost:8080/api/resources
```

The response is a list of resources. Use this endpoint to inspect or debug stored state.

Example:

```json
{
  "apiVersion": "v1",
  "kind": "ResourceList",
  "items": [
    {
      "apiVersion": "v1",
      "kind": "DNSRecord",
      "metadata": {
        "name": "example",
        "namespace": "default"
      },
      "spec": {
        "hostname": "example.test",
        "address": "192.168.1.10"
      }
    }
  ]
}
```

---

# Filter Resources by Kind

Use the `kind` query argument to return only resources of the selected kind.

Endpoint:

```
GET /api/resources?kind=DNSRecord
```

Example:

```bash
curl 'http://localhost:8080/api/resources?kind=DNSRecord'
```

---

# Filter Resources by API Version

Use the `apiVersion` query argument to return resources with the selected API version.

Example:

```bash
curl 'http://localhost:8080/api/resources?apiVersion=v1'
```

For example, this request returns resources with:

```json
"apiVersion": "v1"
```

---

# Filter Resources by Namespace

Use the `namespace` query argument to return resources in the selected namespace.

Example:

```bash
curl 'http://localhost:8080/api/resources?namespace=default'
```

---

# Combine Resource Filters

You can combine the `apiVersion`, `kind`, and `namespace` filters. The server applies all filters to the same request.

Example:

```bash
curl 'http://localhost:8080/api/resources?apiVersion=v1&kind=DNSRecord&namespace=default'
```

This request selects:

```
apiVersion = v1
kind       = DNSRecord
namespace  = default
```

---

# List Resources of a Specific Kind

Endpoint:

```
GET /api/{apiVersion}/{kind}
```

Use the collection endpoint to list resources for a kind. For example, these requests list DNS records and certificates:

```bash
curl http://localhost:8080/api/DNSRecord
```

The server returns a resource list. For example:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecordList",
  "items": [
    {
      "apiVersion": "v1",
      "kind": "DNSRecord",
      "metadata": {
        "name": "example",
        "namespace": "default"
      },
      "spec": {
        "hostname": "example.test",
        "address": "192.168.1.10"
      }
    }
  ]
}
```

---

# List a Kind in a Namespace

Add the `namespace` query argument to list resources in one namespace. For example:

```bash
curl 'http://localhost:8080/api/DNSRecord?namespace=default'
```

---

# Create a Resource

Endpoint:

```
POST /api/{apiVersion}/{kind}
```

Example:

```bash
curl -X POST -H 'Content-Type: application/json' http://localhost:8080/api/DNSRecord \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "metadata": {
      "name": "example",
      "namespace": "default"
    },
    "spec": {
      "hostname": "example.test",
      "address": "192.168.1.10"
    }
  }'
```

The API server validates `spec` against the registered `DNSRecord` schema. If validation succeeds, it creates the resource, assigns a UID, sets generation to `1`, assigns a resource version, and returns HTTP `201 Created` with the resource.

The server generates these fields:

```
UID
Generation
ResourceVersion
```

If `spec` does not match the registered schema, the API server returns HTTP `400 Bad Request`. It does not store the resource or publish an event.

---

# Create a Certificate

Example:

```bash
curl -X POST -H 'Content-Type: application/json' http://localhost:8080/api/Certificate \
  -d '{
    "apiVersion": "v1",
    "kind": "Certificate",
    "metadata": {
      "name": "example",
      "namespace": "default"
    },
    "spec": {
      "hostname": "example.test",
      "issuer": "acme"
    }
  }'
```

After a successful create, the API server publishes an event to the Certificate stream for the certificate controller.

---

# Read One Resource

Endpoint:

```
GET /api/{apiVersion}/{kind}/{name}
```

Example:

```bash
curl http://localhost:8080/api/DNSRecord/example
```

Example response:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "metadata": {
    "name": "example",
    "namespace": "default",
    "uid": "7f3f0a7e-...",
    "generation": 1,
    "resourceVersion": 1
  },
  "spec": {
    "hostname": "example.test",
    "address": "192.168.1.10"
  }
}
```

For a namespaced resource, add the `namespace` query argument:

```bash
curl 'http://localhost:8080/api/DNSRecord/example?namespace=default'
```

---

# Update a Resource

Endpoint:

```
PUT /api/{apiVersion}/{kind}/{name}
```

Example:

```bash
curl -X PUT -H 'Content-Type: application/json' http://localhost:8080/api/DNSRecord/example \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "metadata": {
      "name": "example",
      "namespace": "default"
    },
    "spec": {
      "hostname": "example.test",
      "address": "192.168.1.20"
    }
  }'
```

If the resource exists and `spec` passes schema validation, the server updates it. If validation fails, it returns HTTP `400 Bad Request`, leaves the resource unchanged, and publishes no event. A change to `spec` increments the generation.

For example:

```
generation 1
       |
       | spec changed
       v
generation 2
```

The update also changes the resource version and publishes a `MODIFIED` event.

---

# Delete a Resource

Endpoint:

```
DELETE /api/{apiVersion}/{kind}/{name}
```

Example:

```bash
curl -X DELETE http://localhost:8080/api/DNSRecord/example
```

For a namespaced resource, include the `namespace` query argument:

```bash
curl -X DELETE 'http://localhost:8080/api/DNSRecord/example?namespace=default'
```

If the resource exists, the server removes it and publishes a `DELETED` event. The controller uses that event to remove the corresponding external state. For example:

```
DNSRecord deleted
       |
       v
DNS controller receives DELETED
       |
       v
DNS record removed from DNS server
```

---

# Watch Resources

The watch API is:

```
GET /api/watch
```

The connection remains open and streams matching events when resources change.

Example:

```bash
curl -N http://localhost:8080/api/watch
```

The `-N` option prevents curl from buffering the response, so events appear as the server sends them.

---

# Watch a Specific Kind

Example:

```bash
curl -N 'http://localhost:8080/api/watch?kind=DNSRecord'
```

This request watches only `DNSRecord` resources.

---

# Watch by API Version

Example:

```bash
curl -N 'http://localhost:8080/api/watch?apiVersion=v1'
```

This request watches resources with API version `v1`.

---

# Watch by Namespace

Example:

```bash
curl -N \
  'http://localhost:8080/api/watch?namespace=default'
```

This request watches resources in the `default` namespace.

---

# Combine Watch Filters

Example:

```bash
curl -N \
  'http://localhost:8080/api/watch?apiVersion=v1&kind=DNSRecord&namespace=default'
```

This request filters by:

```
apiVersion = v1
kind       = DNSRecord
namespace  = default
```

---

# Watch Events

The HTTP watch API uses newline-delimited JSON (NDJSON). Each line contains one JSON event object.

Example:

```json
{"type":"ADDED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"},"spec":{"hostname":"example.test","address":"192.168.1.10"}}}
```

A later event appears on another line:

```json
{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"},"spec":{"hostname":"example.test","address":"192.168.1.20"}}}
```

A delete event uses the same structure:

```json
{"type":"DELETED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"}}}
```

The event structure is:

```go
type WatchEvent struct {
    Type   EventType `json:"type"`
    Object Resource  `json:"object"`
}
```

The event types are:

```
ADDED
MODIFIED
DELETED
```

## JetStream Event Subjects

Each kind publishes to `events.<kind-token>`. The token is lowercase; a trailing `Record` is omitted, so the built-in kinds use:

| Kind | Stream | Subject |
| --- | --- | --- |
| `DNSRecord` | `EVENTS_DNS` | `events.dns` |
| `Certificate` | `EVENTS_CERTIFICATE` | `events.certificate` |

Subscribe to all kinds with the NATS wildcard `events.*`, or filter one kind with `events.dns` or `events.certificate`. Other characters in kind names are percent-encoded so each kind remains one subject token. API versions of the same kind share its stream and subject; event payloads retain `apiVersion` so consumers can distinguish versions.

The per-kind streams are durable and retain messages for seven days. A normal NATS wildcard subscription receives live publications; clients needing replay or acknowledgments should create a durable JetStream consumer for each relevant kind stream.

Streams created by earlier versions used hashed names. On upgrade, the API server reuses an existing stream for its subject so retained events are not lost; those legacy names remain until the stream is explicitly migrated or deleted. Seven-day retention expires messages, not the stream itself.

---

# Initial Watch State

When an HTTP client opens `/api/watch`, the API server sends matching resources as `ADDED` events and then streams new events as NDJSON. This snapshot behavior applies to HTTP clients. Controllers use durable JetStream consumers and do not receive an HTTP snapshot.

For example, assume the API server already contains:

```
DNSRecord/example
DNSRecord/test
```

An HTTP client opens a DNSRecord watch. The server sends:

```
ADDED DNSRecord/example
ADDED DNSRecord/test
```

The connection remains open after the initial snapshot and carries new matching events.

If a new resource is created:

```
ADDED DNSRecord/new
```

is sent.

If an existing resource changes:

```
MODIFIED DNSRecord/example
```

is sent.

If a resource is deleted:

```
DELETED DNSRecord/test
```

is sent.

Controllers recover through their durable JetStream consumer position, not through this HTTP snapshot.

---

# Controller Architecture

A controller is a separate Go program. It registers its resource kind through the registration stream, then consumes events from the kind's durable JetStream stream. It reconciles each event with the external system that it manages.

```
Register kind and consume events
      |
      v
Receive event
      |
      v
Reconcile desired state
      |
      v
Change external system
```

Controllers must not depend on one another being online. For example:

```
DNS controller
    |
    +-- API server
    |
    +-- DNS system
```

The DNS controller does not call the certificate controller, and the certificate controller does not call the DNS controller. If controllers need to exchange information, they should use resources.

---

# The Reconciliation Model

A controller should be level-triggered: it should inspect the current desired state and make the external system match it, rather than depend only on the event that caused a change.

For example:

```
Desired:

example.test -> 192.168.1.10
```

The DNS controller checks the DNS system and compares it with the desired record.

If the DNS system already contains:

```
example.test -> 192.168.1.10
```

the controller does not need to change it.

If it contains:

```
example.test -> 192.168.1.20
```

the controller changes it.

If it does not contain the record, the controller creates it.

This makes reconciliation idempotent.

---

# Idempotence

A reconciliation function must be safe to run more than once. For example:

```
reconcile DNSRecord/example
reconcile DNSRecord/example
reconcile DNSRecord/example
```

these calls should produce the same final state as one call.

Do not write controllers that assume:

```
one event = one required action
```

Events can be repeated, controllers can restart, and connections can fail. A controller must be able to reconcile the same resource again.

---

# Controller Restart

A controller can stop while the API server continues to store resources and publish events. Its durable JetStream consumer records acknowledged messages and resumes from its saved position when the controller restarts.

```
API server
    |
    +-- DNSRecord/example
    +-- DNSRecord/test
```

The controller acknowledges an event only after reconciliation succeeds. If it stops before acknowledging an event, JetStream can deliver that event again. Since delivery is at least once, reconciliation must be idempotent. The controller can also read the current resource state from the API server when it needs to reconcile the latest desired state.

---

# Event Retention

Per-kind streams retain messages for seven days. JetStream removes expired messages, so a controller that remains offline beyond the retention period can miss older events. The controller must use the API server's current resource state to restore external state after a long outage.

---

# DNS Controller

The DNS controller registers this resource kind:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true
}
```

It consumes events from the durable JetStream stream for `v1/DNSRecord`. The controller does not call the API server for registration or event delivery.

The controller passes each `WatchEvent` to its reconciliation function:

```go
func reconcile(event protocol.WatchEvent) error {
    resource := event.Object

    switch event.Type {
    case protocol.Added:
        return reconcileDNSRecord(resource)

    case protocol.Modified:
        return reconcileDNSRecord(resource)

    case protocol.Deleted:
        return removeDNSRecord(resource)

    default:
        return fmt.Errorf("unknown event type %q", event.Type)
    }
}
```

The example logs reconciliation activity. A production controller can use the same event handling to update a DNS server.

---

# Certificate Controller

The certificate controller uses the same registration and event-consumer pattern. It registers:

```json
{
  "apiVersion": "v1",
  "kind": "Certificate",
  "resource": "certificates",
  "namespaced": true
}
```

It consumes events from the durable `v1/Certificate` stream:

```
ADDED
MODIFIED
DELETED
```

events.

For `ADDED` and `MODIFIED`, it reconciles the requested certificate. For `DELETED`, it removes or revokes the corresponding external state, as required by the certificate system.

---

# How to Create a New Controller

You can create a new controller without changing the API server.

For example, assume you want a controller that manages DHCP reservations.

Create:

```
dhcp-controller/
└── main.go
```

Choose a resource kind:

```
DHCPReservation
```

Choose its resource name:

```
dhcReservations
```

A clearer resource name would normally be:

```
dhcpreservations
```

Register the kind:

```json
{
  "apiVersion": "v1",
  "kind": "DHCPReservation",
  "resource": "dhcpreservations",
  "namespaced": true
}
```

The API server does not need any DHCP-specific code.

---

# Define the Resource

Decide what the desired state should contain.

For example:

```json
{
  "apiVersion": "v1",
  "kind": "DHCPReservation",
  "metadata": {
    "name": "printer",
    "namespace": "default"
  },
  "spec": {
    "macAddress": "00:11:22:33:44:55",
    "address": "192.168.1.50",
    "hostname": "printer"
  }
}
```

The controller owns the meaning of:

```
macAddress
address
hostname
```

The API server validates `spec` against the schema registered for this kind, then stores the resource.

---

# Register the New Kind

The controller should register the kind when it starts.

The Go structure includes the schema for `spec`:

```go
var dhcpReservationKind = protocol.ResourceKind{
  APIVersion: "v1",
  Kind:       "DHCPReservation",
  Resource:   "dhcpreservations",
  Namespaced: true,
  Schema: map[string]any{
    "type":     "object",
    "required": []string{"macAddress", "address", "hostname"},
    "properties": map[string]any{
      "macAddress": map[string]any{"type": "string", "minLength": 1},
      "address":    map[string]any{"type": "string", "minLength": 1},
      "hostname":   map[string]any{"type": "string", "minLength": 1},
    },
    "additionalProperties": false,
  },
}
```

Publish it to the kind registration stream and wait for the API server's acknowledgment:

```go
broker, err := messaging.Connect(messaging.URLFromEnv(), "dhcp-controller")
if err != nil {
  return err
}
defer broker.Close()

if err := broker.RegisterKind(ctx, dhcpReservationKind); err != nil {
  return err
}
```
## Consume Events

Use a stable durable name for the logical controller. Replicas of that controller should use the same name to share work; other clients should use their own durable name to receive an independent copy.

```go
durable := messaging.StableDurableName(
  "dhcp-controller",
  dhcpReservationKind.APIVersion,
  dhcpReservationKind.Kind,
)
return broker.RunKindEvents(
  ctx,
  dhcpReservationKind.APIVersion,
  dhcpReservationKind.Kind,
  durable,
  reconcile,
)
```

---

# Implement Reconciliation

Start with:

```go
func reconcile(event protocol.WatchEvent) error {
    resource := event.Object

    switch event.Type {
    case protocol.Added:
        return reconcileDHCPReservation(resource)

    case protocol.Modified:
        return reconcileDHCPReservation(resource)

    case protocol.Deleted:
        return removeDHCPReservation(resource)

    default:
        return fmt.Errorf(
            "unknown event type %q",
            event.Type,
        )
    }
}
```

Then implement the desired-state logic.

For example:

```go
func reconcileDHCPReservation(
    resource protocol.Resource,
) error {
    macAddress, _ := resource.Spec["macAddress"].(string)
    address, _ := resource.Spec["address"].(string)
    hostname, _ := resource.Spec["hostname"].(string)

    log.Printf(
        "reconciling DHCP reservation %s: MAC=%s address=%s hostname=%s",
        resource.Metadata.Name,
        macAddress,
        address,
        hostname,
    )

    // Configure the DHCP server here.

    return nil
}
```

The example uses type assertions because `Spec` is:

```go
map[string]any
```

A real controller should validate the values before using them.

---

# Handle Deletion

Deletion is different from normal reconciliation.

The resource no longer exists in the API server after deletion.

The delete event still contains the deleted resource.

This gives the controller the information that it needs to remove external state.

Example:

```go
func removeDHCPReservation(
    resource protocol.Resource,
) error {
    macAddress, _ := resource.Spec["macAddress"].(string)

    log.Printf(
        "removing DHCP reservation %s for MAC %s",
        resource.Metadata.Name,
        macAddress,
    )

    // Remove the reservation from the DHCP server.

    return nil
}
```

Do not ignore delete events.

If you ignore them, external resources can remain after the Controlplane resource is deleted.

---

# Add the Controller to the Taskfile

Add a build task:

```yaml
build-dhcp-controller:
  desc: Build the DHCP controller
  cmds:
    - go build -o {{.BIN_DIR}}/dhcp-controller ./dhcp-controller
```

Add a run task:

```yaml
run-dhcp:
  desc: Run the DHCP controller
  cmds:
    - go run ./dhcp-controller
```

Add the new controller to the main build task:

```yaml
build:
  desc: Build all components
  cmds:
    - task: build-api-server
    - task: build-dns-controller
    - task: build-certificate-controller
    - task: build-dhcp-controller
```

---

# The Default Task

The default Taskfile task lists available tasks.

```bash
task
```


---

# Building the Project

Build the API server, `gubctl`, and both controllers:

```bash
task build
```

The binaries are written to `bin/`:

```
bin/
├── api-server
├── gubctl
├── dns-controller
├── certificate-controller
```

---

# Running the API Server

Run the API server and its local JetStream server together:

```bash
task run
```

The API server and controllers use `NATS_URL`, which defaults to `nats://localhost:4222`.

JetStream messages are stored under `.nats/jetstream` and survive NATS server restarts. Keep this directory to preserve messages; deleting it removes the local JetStream data.

The local config uses separate `admin` and application users. The application account is restricted to Controlplane registration, event, inbox, and JetStream API subjects. Local-only development defaults are used when credentials are unset; set `NATS_ADMIN_PASSWORD`, `NATS_APP_USERNAME`, and `NATS_APP_PASSWORD` before startup for non-local use. Standalone API/controller clients use `NATS_USERNAME` and `NATS_PASSWORD`; these must match the application account. Monitoring is available on `127.0.0.1:8222`.

For a multi-terminal setup, start NATS separately with `task run-nats`, then start the API server and controllers in their own terminals. Do not run both NATS tasks at once.

The API server listens on:

```
localhost:8080
```

The root path is not an API endpoint.

Therefore:

```bash
curl http://localhost:8080/
```

returns:

```
404 page not found
```

This is expected.

Use an API endpoint instead:

```bash
curl http://localhost:8080/api/kinds
```

---

# gubctl Client

`gubctl` is the Controlplane command-line client. It follows familiar kubectl-style commands and reads resource and kind definitions from YAML.

Build it with `task build-gubctl` to create `bin/gubctl`, or run a one-off command with `task run-gubctl -- get kinds`.

List API endpoints and registered kinds:

```bash
gubctl api-resources
gubctl get kinds
gubctl get namespaces
```

List resources, get one resource, or select a namespace:

```bash
gubctl get DNSRecord -n default
gubctl get DNSRecord example -n default -o yaml
```

Describe a resource or a kind:

```bash
gubctl describe DNSRecord example -n default
gubctl describe DNSRecord/example -n default
gubctl describe kind DNSRecord
```

Create or apply a resource definition:

```yaml
apiVersion: v1
kind: DNSRecord
metadata:
  name: example
  namespace: default
spec:
  hostname: example.test
  address: 192.0.2.10
```

```bash
gubctl create -f dns-record.yaml
gubctl apply -f dns-record.yaml
```

Kind definitions use the same YAML form as the API, including `resource`, `namespaced`, and optional `schema` fields. `apply` creates missing definitions and updates existing ones. `delete kind KIND` removes a kind only after its resources have been deleted.

Delete resources by name or from a YAML file:

```bash
gubctl delete DNSRecord example -n default
gubctl delete -f dns-record.yaml
```

Watch all events, a kind, or one resource. `--type` can be repeated or given a comma-separated list:

```bash
gubctl watch
gubctl watch DNSRecord
gubctl watch DNSRecord/example -n default --type ADDED,MODIFIED
```

Use `--server` or `gubctl_SERVER` to select the API server. Output formats are `table`, `yaml`, and `json`; use `-o yaml` or `-o json`.

---

# Running a Controller

Run the DNS controller:

```bash
task run-dns
```

Run the certificate controller:

```bash
task run-certificate
```

A controller connects to `NATS_URL`, registers its resource kind, waits for the API server's acknowledgment, then starts its durable consumer.

A normal controller log looks similar to:

```
DNS controller started
registered resource kind v1/DNSRecord
```

---

# Test the API Manually

Start the API server with `task run`, then register a DNS kind if a controller has not already registered it:

```bash
curl -X POST -H 'Content-Type: application/json' http://localhost:8080/api/kinds \
  -d '{
    "apiVersion": "v1",
```

Create a resource:

```bash
curl -X POST -H 'Content-Type: application/json' http://localhost:8080/api/DNSRecord \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "metadata": {
      "name": "example",
      "namespace": "default"
    },
    "spec": {
      "hostname": "example.test",
      "address": "192.168.1.10"
    }
  }'
```
    ```bash
    task run-nats
    ```

    Then run the API server in another terminal:

    ```bash
    task run
    ```

Read it:

```bash
curl 'http://localhost:8080/api/DNSRecord/example?namespace=default'
```

Update it:

```bash
curl -X PUT -H 'Content-Type: application/json' http://localhost:8080/api/DNSRecord/example \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "metadata": {
      "name": "example",
      "namespace": "default"
    },
    "spec": {
      "hostname": "example.test",
      "address": "192.168.1.20"
    }
  }'
```

Delete it:

```bash
curl -X DELETE 'http://localhost:8080/api/DNSRecord/example?namespace=default'
```

---

# HTTP Status Codes

The current API uses normal HTTP status codes.

| Status | Meaning                        |
| ------ | ------------------------------ |
| 200    | Request completed successfully |
| 201    | Resource or kind created       |
| 204    | Kind deleted                   |
| 400    | Invalid request, schema, or resource spec |
| 404    | Resource or endpoint not found |
| 405    | HTTP method is not supported   |
| 409    | Resource already exists        |
| 500    | Internal server error          |

Creating a resource with a name that already exists returns HTTP `409 Conflict`.

---

# API Request Flow

A resource creation stores the resource and an `ADDED` outbox event in one transaction. The API server responds after the transaction commits; a background worker then publishes the event to JetStream.

```
curl
 |
 | POST /api/DNSRecord
 v
API server
 |
 v
HTTP handler
 |
 v
JSON decoder
 |
 v
Store.Create()
 |
+-- generate UID and resourceVersion
+-- store resource and outbox event
 |
 +--> HTTP response
 |
 +--> HTTP watcher broadcast
 |
 +--> Outbox relay --> JetStream --> durable controller consumer
```

The controller receives `ADDED` after the outbox relay publishes the event. JetStream confirms the publish before the relay removes the outbox row.

---

# API Update Flow

An update stores the new resource state and a `MODIFIED` outbox event in one transaction. The API server increments `generation` when `spec` changes and increments `resourceVersion` for each update.

```
PUT /api/DNSRecord/example
          |
          v
     API server
          |
          v
      Store.Update
          |
           +-- preserve UID
           +-- update generation and resourceVersion
           +-- save resource and outbox event
           |
           +--> HTTP 200
           +--> Outbox relay --> JetStream --> durable controller consumer
```

After publication, the controller receives `MODIFIED` and reconciles the new desired state.

---

# API Delete Flow

A delete removes the resource and stores a `DELETED` outbox event in one transaction. The event contains the deleted resource so the controller can remove its external state.

```
DELETE
  |
  v
API server
  |
  v
Store.Delete
  |
  +-- remove resource and save outbox event
  |
  +--> HTTP response
  +--> Outbox relay --> JetStream --> durable controller consumer
```

After publication, the controller receives the deleted resource and removes its external state.

---

# API Server Does Not Run Controllers

The API server does not contain code such as:

```go
runDNSController()
```

or:

```go
runCertificateController()
```

The API server remains generic, so a new controller can be added without changing it.

For example:

```
API server
    |
    +-- DNS controller
    |
    +-- Certificate controller
    |
    +-- DHCP controller
    |
    +-- Firewall controller
    |
    +-- Load balancer controller
```

Each controller can be developed and deployed separately.

---

# Controllers Communicate Through Resources

If a certificate controller creates a resource that another controller needs, they should exchange information through resources instead of calling one another directly.

Instead:

```
Certificate Controller
        |
        v
     Resource
        |
        v
Other Controller
```

This keeps controllers independent and lets you replace one controller without changing another.

---

# Example Controller Design

A controller normally has these components:

```
main
 |
 +-- register kind
 |
 +-- consume durable stream
       |
       +-- decode event
       |
       +-- reconcile
              |
              +-- Added
              +-- Modified
              +-- Deleted
```

A larger controller can separate these into multiple Go files:

```
dns-controller/
├── main.go
├── controller.go
├── reconcile.go
├── client.go
└── types.go
```

Use one file for a small controller. Split the code when it becomes difficult to understand.

---

# Resource Validation

The API server validates `spec` against the JSON Schema registered for the resource kind. The schema can check required fields, JSON types, allowed values, and other structural constraints. A schema-invalid create or update returns HTTP `400 Bad Request` without changing stored state or sending a watch event.

The schema does not replace controller validation. A controller must still check domain-specific rules and constraints that depend on the target system. For example, a schema can require an address string, while the DNS controller checks whether that string is a valid address for its provider.

Do not assume that data accepted by a schema is valid for every external system. A controller should validate:

```
required fields
data types
allowed values
formats
relationships
external constraints
```

---

# Controller Error Handling

A reconciliation error should not normally terminate the controller. Log the error and allow JetStream to redeliver the event after the configured delay.

For example:

```go
if err := reconcile(event); err != nil {
    log.Printf("reconcile failed: %v", err)
}
```

The consumer remains active after a reconciliation error. It sends a delayed negative acknowledgment, and JetStream redelivers the event. If the consumer stops, the controller reconnects to NATS and resumes its durable consumer.

---

# Logging

Include the resource kind, name, namespace, generation, and resource version in logs. For example:

```
RECONCILE DNSRecord/example generation=2 resourceVersion=7
```

These fields help identify the resource during troubleshooting. Do not log sensitive information.

---

# Resource Version and Generation

`generation` represents the desired configuration version. It changes when `spec` changes.

Example:

```
generation 1
generation 2
generation 3
```

`resourceVersion` identifies a stored revision and changes when the resource is created or updated.

The distinction is:

```
generation
    |
    +-- desired configuration changed

resourceVersion
    |
    +-- stored resource changed
```

A controller can compare `generation` to determine whether it processed the latest desired configuration. It can use `resourceVersion` to identify the stored revision it observed.

---

# Resource Storage and Event Delivery

The API server stores resources, kinds, schemas, and pending outbox events in SQLite or PostgreSQL. A resource mutation and its event enter the database in one transaction. The API server publishes pending events to the matching JetStream stream and removes each outbox row after JetStream confirms the publish. A crash can cause a duplicate publish, so consumers must handle events idempotently.

Per-kind JetStream streams use file storage and retain messages for seven days. Controllers use durable consumers and acknowledge events after reconciliation succeeds. Unacknowledged events can be delivered again. Events older than the retention period are removed.

---

# HTTP Watch Behavior

`GET /api/watch` sends the current matching resources as `ADDED` events, then streams new changes as NDJSON. The endpoint stores active connections and watcher state in API-server memory. An API-server restart closes these connections, so HTTP clients must reconnect and receive a new snapshot. Controllers do not use this endpoint; they consume durable JetStream streams.

---

# API Server Persistence Model

The API server persists state in SQL and publishes resource events to JetStream:

```
REST clients and gubctl
        |
        | HTTP
        v
    API server -------> NATS JetStream <------- Controllers
        |                                      |
        v                                      v
   ResourceStore                         External systems
        |
        +-- SQLite or PostgreSQL: resources, kinds, and outbox
        +-- API-server memory: active HTTP watchers
```

The controller API does not depend on which storage backend is selected.

---

# Persistent Storage

The API server uses a persistent storage layer to preserve resource state across restarts. It depends on the `ResourceStore` interface, not database-specific code. The current implementations support:

* SQLite
* PostgreSQL

SQLite is the default backend.

### Storage Architecture

The API server uses this architecture:

```
                         API Server
                             |
                             v
                      ResourceStore
                        interface
                             |
                  +----------+----------+
                  |                     |
                  v                     v
               SQLite              PostgreSQL
                  |                     |
                  v                     v
          controlplane.db       PostgreSQL database
```

The API server validates and stores resources, writes outbox events, serves the REST API, and publishes events to JetStream. Controllers use JetStream for kind registration and event delivery; they do not access the database directly.

```
API clients --HTTP--> API server --ResourceStore--> Database
               |
               +--NATS JetStream--> Controllers
```

This separation lets the API server change storage implementations without requiring controller changes.

### Resource Storage

Resources are stored using the generic `protocol.Resource` structure:

```go
type Resource struct {
    APIVersion string         `json:"apiVersion"`
    Kind       string         `json:"kind"`
    Metadata   Metadata       `json:"metadata"`
    Spec       map[string]any `json:"spec,omitempty"`
    Status     map[string]any `json:"status,omitempty"`
}
```

The storage layer stores `spec` and `status` as JSON and does not interpret them. It can store different resource kinds through the same mechanism, including:

```
DNSRecord
Certificate
TerraformStack
DHCPReservation
PKIResource
```

The API server validates `spec` against the kind's registered JSON Schema, then stores it as JSON. The owning controller interprets the fields and manages `status`.

### Resource Identity

A resource is uniquely identified by `apiVersion`, `kind`, `namespace`, and `name`. For example:

```
apiVersion
kind
namespace
name
```

For example:

```
v1 / DNSRecord / default / example
```

Different kinds can therefore use the same name without conflict. For example:

```
v1 / DNSRecord / default / example
v1 / Certificate / default / example
```

are different resources.

The API server assigns each resource a unique `uid` at creation. The UID remains stable for the resource's lifetime.

### Resource Version

Every resource receives a monotonically increasing `resourceVersion`. For example:

```
resourceVersion = 1
resourceVersion = 2
resourceVersion = 3
resourceVersion = 4
```

The storage layer generates and persists the version when a resource is created or updated. It is not reset when the API server restarts, so controllers can use it to identify the stored revision.

### Generation

Each resource has a `generation` that starts at `1` and increases when its desired configuration changes. For example:

```
Create:
generation = 1

Update:
generation = 2

Update:
generation = 3
```

Controllers can use the generation to determine whether they have reconciled the latest desired configuration. The storage layer maintains the value but does not interpret it.

### Resource Kinds

Resource kinds are persisted. Controllers publish definitions to the JetStream registration stream; the API server validates and stores each definition, creates its event stream, and acknowledges registration. Administrative clients can also register a kind through:

```
POST /api/kinds
```

For example:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true
}
```

The storage backend keeps kind definitions across API-server restarts. Clients can list registered kinds with:

```
GET /api/kinds
```

This lets clients discover available resource types without a hard-coded controller list in the API server.

### SQLite

SQLite is the default storage backend.

It is useful for:

* Local development
* Testing
* Single-node deployments
* Small installations
* Development environments where running PostgreSQL is unnecessary

Start the API server with the default configuration:

```bash
go run ./api-server
```

The API server creates:

```
controlplane.db
```

The database location can be changed with:

```bash
export SQLITE_PATH=/var/lib/controlplane/controlplane.db
go run ./api-server
```

SQLite uses a persistent database file, so resources remain available after the API server stops and starts again.

The SQLite implementation enables WAL mode to improve concurrent read/write behavior.

### PostgreSQL

PostgreSQL is supported for deployments that require a separate database server.

PostgreSQL is useful when the API server requires:

* A database server separate from the API server
* Multiple API-server instances
* Centralized database management
* Database backups
* Higher concurrency
* A database service managed independently of the application

Select PostgreSQL with:

```bash
export STORAGE=postgres
```

The connection string is supplied through `POSTGRES_DSN`:

```bash
export POSTGRES_DSN="postgres://controlplane:secret@localhost:5432/controlplane?sslmode=disable"
```

Then start the API server:

```bash
go run ./api-server
```

The API server creates the required tables automatically when it starts.

### Database Schema

Both storage implementations maintain the same logical data model.

The main resource table contains fields equivalent to:

```
resources
------------------------------------------------
api_version
kind
namespace
name
uid
generation
resource_version
labels
annotations
spec
status
```

The `spec` and `status` fields are stored as JSON.

Labels and annotations are also stored as structured data.

A separate table stores registered resource kinds:

```
resource_kinds
------------------------------------------------
api_version
kind
resource
namespaced
schema
```

The `schema` column contains the JSON Schema used to validate `spec`. Older database files receive an empty schema during migration, which leaves their existing kinds unrestricted until a controller registers an updated definition.

A metadata table stores the current resource-version counter:

```
metadata
------------------------------------------------
key
value
```

The resource-version counter is persistent. It is not maintained only in API-server memory.

### Why the API Server Does Not Store Controller State

The database stores control-plane state, not private controller implementation state.

For example, a `DNSRecord` might contain:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "metadata": {
    "name": "example"
  },
  "spec": {
    "hostname": "example.com",
    "address": "192.0.2.10"
  }
}
```

The DNS controller reads this resource and makes the external DNS system match the desired state. It does not need a private database with a second copy of the DNS record. The same principle applies to other controllers.

For example:

```
TerraformStack resource
        |
        v
Terraform controller
        |
        v
Terraform
        |
        v
AWS
```

The API server stores the desired `TerraformStack` resource and its status, while Terraform remains responsible for its own state. This prevents multiple systems from becoming competing sources of truth.

### Persistence and Controller Restarts

The API server stores a resource even when its controller is offline. It saves the matching event in the SQL outbox and publishes it to JetStream:

```
User
 |
 | POST DNSRecord
 v
API Server
 |
 v
Database
```

When the controller starts, it registers its kind and resumes its durable consumer:

```
DNS Controller --> NATS JetStream --> API Server
   |                                |
   +-- durable consumer            +-- resource API and database
```

The durable consumer receives retained events that it has not acknowledged. The controller can also query the API server for current state. Controllers reconcile desired state; they do not own it.

### API Server Restart

The API server can restart without losing resources, kinds, or pending outbox events. Before the restart:

```
Database
    |
    +-- DNSRecord/example
    +-- Certificate/example
    +-- TerraformStack/network
```

After the API server starts, it reconnects to the database and JetStream:

```
API Server
    |
    v
Database
    |
    +-- DNSRecord/example
    +-- Certificate/example
    +-- TerraformStack/network
```

The resources remain in the database. After restart, controllers reconnect through their durable JetStream consumers and resume reconciliation.

### Watch State

Resources, kinds, outbox events, and JetStream messages are persistent. Active HTTP watch connections exist only in API-server memory:

```
Database
    |
    +-- Persistent resources
    +-- Persistent resource kinds
    +-- Persistent resource versions

API Server memory
    |
    +-- Active HTTP connections
    +-- Active watchers
```

When the API server stops, HTTP watch connections close, and HTTP clients must reconnect. Controllers reconnect to JetStream and resume their durable consumers. After the seven-day retention period, a controller must query current resource state to restore external state.

### Storage Abstraction

The `ResourceStore` interface is independent of the database implementation:

Conceptually:

```go
type ResourceStore interface {
    Create(...)
    Get(...)
    List(...)
    Update(...)
    Delete(...)

    RegisterKind(...)
    DeleteKind(...)
    ListKinds(...)
    ListPendingEvents(...)
    MarkEventPublished(...)
}
```

The API server depends on this interface rather than SQLite or PostgreSQL directly. You can add another storage implementation without changing the API layer. For example:

```
ResourceStore
     |
     +-- SQLite
     |
     +-- PostgreSQL
     |
     +-- Future implementation
```

The protocol and controller APIs remain unchanged.

### Selecting the Storage Backend

SQLite is selected by default:

```bash
go run ./api-server
```

Or explicitly:

```bash
STORAGE=sqlite go run ./api-server
```

A custom SQLite database can be selected with:

```bash
STORAGE=sqlite \
SQLITE_PATH=/var/lib/controlplane/controlplane.db \
go run ./api-server
```

PostgreSQL can be selected with:

```bash
STORAGE=postgres \
POSTGRES_DSN="postgres://controlplane:secret@localhost:5432/controlplane?sslmode=disable" \
go run ./api-server
```

The controller configuration does not change when the storage backend changes.

### Storage and Watch Events

The database is the source of truth for resources. A resource operation writes the resource and its outbox event in one database transaction. The API server then publishes the event to JetStream:

```
Client
  |
  | POST / PUT / DELETE
  v
API Server
  |
  | transaction
  v
Persistent Storage
  |
  | commit
  v
API server --JetStream publish--> Per-kind event stream --> Durable consumers
```

The database commit occurs before JetStream publication. The outbox preserves events that are waiting for publication. If a controller misses an event after the seven-day retention period, it can query the API server and reconcile the current state.

### Possible Storage Improvements

The implementation provides persistent resources, kinds, and an outbox. HTTP watch connections remain in memory. Possible improvements include:

* PostgreSQL `LISTEN/NOTIFY` for API-server instances that share one PostgreSQL database
* Optimistic concurrency using resource versions
* Database connection pooling configuration
* Automatic database migrations
* Database backup and restore tooling
* High-availability API-server deployments

These improvements do not require changing the resource model or controller API. The current event flow is:

```
                Persistent Desired State
                         |
                         v
                    API Server
                         |
                 +-------+-------+
                 |               |
                REST API     JetStream
                 |               |
                 v               v
             Controllers    Controllers
                 |
                 v
          External Systems
```

The database provides durable desired state. The API server provides the resource API and event publisher. JetStream provides retained event delivery. Controllers provide reconciliation logic.

---

# Security Considerations

The NATS server uses separate administrator and application accounts. The application account has permissions for Controlplane registration, event, inbox, and JetStream API subjects. The local passwords are development defaults; set `NATS_ADMIN_PASSWORD`, `NATS_APP_USERNAME`, and `NATS_APP_PASSWORD` before using the server outside local development. NATS TLS is not enabled by default.

The HTTP API does not provide authentication, authorization, or TLS. It listens on all network interfaces by default. Do not expose it to an untrusted network. A production deployment must define access rules for resource CRUD, watches, and kind registration.

The current API server does not provide production-grade security features such as:

```
TLS
authentication
authorization
audit logging
network access control
```

---

# Production Improvements

The current implementation can be extended with:

```
authentication
authorization
TLS
optimistic concurrency
controller work queues
retry backoff
health endpoints
metrics
structured logging
audit logging
leader election
graceful shutdown
configuration management
```

Add these features to meet deployment requirements while keeping controller-specific logic outside the API server.

---

# Testing Controllers

A controller should be tested independently. The project provides API CRUD integration scripts through:

```bash
task test-controllers
```

The scripts discover registered kinds, then:

```
create resources
update resources
delete resources
verify API responses
clean up resources
```

A controller behavior test should verify reconciliation in the external system, not only an HTTP status. The current scripts verify resource CRUD and cleanup; they do not assert changes to a real external system.

For example:

```
Create DNSRecord
       |
       v
DNS controller receives ADDED
       |
       v
DNS state created
       |
       v
Update DNSRecord
       |
       v
DNS controller receives MODIFIED
       |
       v
DNS state updated
       |
       v
Delete DNSRecord
       |
       v
DNS controller receives DELETED
       |
       v
DNS state removed
```

---

# Recommended Controller Development Process

Use this sequence to create a controller:

1. Define the resource kind, API version, specification, schema, and optional status fields.
2. Register the kind with `messaging.Client.RegisterKind` and wait for the API server's acknowledgment.
3. Create a stable durable consumer for the kind's event stream.
4. Reconcile `ADDED`, `MODIFIED`, and `DELETED` events. Acknowledge an event only after reconciliation succeeds.
5. Make reconciliation idempotent and validate domain-specific rules that the schema cannot express.
6. Retry external-system failures without terminating the controller.
7. Test create, update, delete, restart, and failure behavior. Include resource identity in log messages.
8. Add build and run tasks to the Taskfile.

---

# Complete Controller Pattern

A controller connects to NATS, registers its kind, and starts a durable consumer. The consumer acknowledges an event only after reconciliation succeeds.

```go
import (
  "context"
  "fmt"

  "controlplane/messaging"
  "controlplane/protocol"
)

var resourceKind = protocol.ResourceKind{
  APIVersion: "v1",
  Kind:       "ExampleResource",
  Resource:   "exampleresources",
  Namespaced: true,
}

func run(ctx context.Context) error {
  broker, err := messaging.Connect(messaging.URLFromEnv(), "example-controller")
  if err != nil {
    return err
  }
  defer broker.Close()

  if err := broker.RegisterKind(ctx, resourceKind); err != nil {
    return err
  }

  durable := messaging.StableDurableName(
    "example-controller",
    resourceKind.APIVersion,
    resourceKind.Kind,
  )
  return broker.RunKindEvents(
    ctx,
    resourceKind.APIVersion,
    resourceKind.Kind,
    durable,
    reconcile,
  )
}

func reconcile(event protocol.WatchEvent) error {
  switch event.Type {
  case protocol.Added, protocol.Modified:
    return reconcileResource(event.Object)
  case protocol.Deleted:
    return deleteResource(event.Object)
  default:
    return fmt.Errorf("unknown event type %q", event.Type)
  }
}
```

`reconcileResource` and `deleteResource` must be idempotent. They contain the application-specific work that makes the external system match the resource.

---

# Common Mistakes

## Put controller logic in the API server

Do not do this:

```go
if resource.Kind == "DNSRecord" {
    updateDNS()
}
```

The API server must remain generic.

---

## Depend on event history

JetStream provides at-least-once delivery and retains events for seven days. Do not assume exactly-once delivery or indefinite history. A controller must be able to reconstruct desired state from the current resource.

---

## Make reconciliation non-idempotent

To avoid duplicate external objects, check the actual external state first and change it only when necessary.

---

## Ignore delete events

If a controller creates external state, it must normally remove that state when the resource is deleted.

---

## Stop the controller on one external error

External systems can fail temporarily. A controller should remain active and retry the operation.

---

## Assume `spec` is valid

The API server validates `spec` against the kind's registered schema. A controller must also check domain-specific rules and external-system constraints.

---

## Store controller state only in memory

A controller may restart, so do not keep the only copy of desired state in controller memory. Store desired state in an API resource.

---

# Recommended Mental Model

When developing a controller, compare desired state in the resource with actual state in the external system. Change the external system until the two states match:

```
Resource
   |
   | says what the user wants
   v
API server
   |
   | stores desired state
   v
Controller
   |
   | compares desired and actual state
   v
External system
```

```
desired state == actual state
```

---

# Complete API Reference

## Kind discovery

```
GET /api/kinds
```

Returns all registered resource kinds.

---

## Kind registration

```
POST /api/kinds
```

Request body:

```json
{
  "apiVersion": "v1",
  "kind": "ExampleResource",
  "resource": "exampleresources",
  "namespaced": true,
  "schema": {
    "type": "object",
    "required": ["name"],
    "properties": {
      "name": { "type": "string" }
    }
  }
}
```

`schema` is optional. When provided, it validates the resource's `spec` on create and update.

## Kind details

Use the kind's API version and name to read, update, or delete a kind:

```
GET    /api/kinds/{apiVersion}/{kind}
PUT    /api/kinds/{apiVersion}/{kind}
DELETE /api/kinds/{apiVersion}/{kind}
```

`PUT` uses the same kind definition as registration. `DELETE` returns a conflict if resources still use the kind.

---

## All resources

```
GET /api/resources
```

Optional query arguments:

```
apiVersion
kind
namespace
```

Example:

```
/api/resources?apiVersion=v1&kind=DNSRecord&namespace=default
```

---

## All resources watch

```
GET /api/watch
```

Optional query arguments:

```
apiVersion
kind
namespace
```

Example:

```
/api/watch?apiVersion=v1&kind=DNSRecord&namespace=default
```

---

## Resource collection

```
GET /api/{apiVersion}/{kind}
```

Optional query argument:

```
namespace
```

Example:

```
GET /api/v1/DNSRecord?namespace=default
```

---

## Create resource

```
POST /api/{apiVersion}/{kind}
```

Request body:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "metadata": {
    "name": "example",
    "namespace": "default"
  },
  "spec": {
    "hostname": "example.test",
    "address": "192.168.1.10"
  }
}
```

---

## Read resource

```
GET /api/{apiVersion}/{kind}/{name}
```

Optional query argument:

```
namespace
```

Example:

```
GET /api/v1/DNSRecord/example?namespace=default
```

---

## Update resource

```
PUT /api/{apiVersion}/{kind}/{name}
```

Request body contains the complete resource.

Example:

```
PUT /api/v1/DNSRecord/example
```

---

## Delete resource

```
DELETE /api/{apiVersion}/{kind}/{name}
```

Optional query argument:

```
namespace
```

Example:

```
DELETE /api/v1/DNSRecord/example?namespace=default
```

---

# Final Architecture

The current request and event paths are:

```
                gubctl and REST clients
                    |
                    | HTTP REST API
                    v
                  API server -------- ResourceStore --------> SQLite or PostgreSQL
                    |
                    | NATS JetStream
                    v
                   Controllers -------- reconcile --------> External systems
```

    HTTP watch clients connect to the API server for a snapshot and live NDJSON events.

The important dependency direction is:

```
protocol
   ^
   |
   +---- API server
   |
   +---- DNS controller
   |
   +---- Certificate controller
   |
   +---- Future controllers
```

The API server does not depend on controller-specific logic. Controllers share the protocol types and use JetStream for registration and event delivery.

---

# Summary

Controlplane stores desired state as resources. The API server provides resource and kind CRUD, schema validation, and resource listing. Each successful resource mutation writes an event to the SQL outbox. The API server publishes outbox events to a per-kind JetStream stream.

Controllers register kinds through the registration stream and consume events through durable JetStream consumers. Delivery is at least once, and streams retain events for seven days. Controllers must make reconciliation idempotent and use the API server's current resource state after outages that exceed event retention.

`gubctl` lists endpoints, manages resources and kinds from YAML files, and watches HTTP events. The separate HTTP watch endpoint sends an initial resource snapshot and then NDJSON events. Its active connections are held in API-server memory.

SQLite is the default storage backend. PostgreSQL is also supported. The NATS configuration uses persistent JetStream storage under `.nats/jetstream` for local runs.
