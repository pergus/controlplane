# Controlplane User Guide

## 1. Introduction

Controlplane is a small and generic control plane written in Go.

The system provides a central API server. The API server stores resources and sends resource changes to controllers.

A controller watches resources and makes changes to an external system.

The design is similar to the controller model used by larger control-plane systems. The implementation is intentionally small. You can use it as a base for infrastructure projects.

This guide explains:

* how the API server works;
* how resources are represented;
* how to use the API;
* all current API endpoints;
* how resource watches work;
* how controllers work;
* how to create a new controller;
* how controllers recover after a restart;
* how to test a controller;
* which parts of the current implementation are suitable for production and which parts need improvement.

The guide uses simple language and short instructions. You do not need advanced Go knowledge to follow it.

---

# 2. System Overview

The system has three main parts:

```text
                    +----------------------+
                    |      API Server      |
                    |                      |
                    | Resource Store       |
                    | Kind Registry        |
                    | REST API             |
                    | Watch API            |
                    +----------+-----------+
                               |
                 +-------------+-------------+
                 |                           |
                 | HTTP API                  | HTTP Watch
                 |                           |
        +--------v--------+         +--------v--------+
        | DNS Controller  |         | Certificate     |
        |                 |         | Controller      |
        +--------+--------+         +--------+--------+
                 |                           |
                 v                           v
             DNS system                 PKI / ACME
```

The API server is the central component.

Controllers are separate programs.

The API server does not contain DNS logic.

The API server does not contain certificate logic.

The API server does not call controllers directly.

Instead, the API server stores resources.

A controller watches the resources that it understands.

The controller then reconciles the desired state with the real state of the external system.

This separation is one of the most important design rules in Controlplane.

---

# 3. Project Structure

The project has this structure:

```text
controlplane/
├── Taskfile.yml
├── go.mod
│
├── protocol/
│   └── resource.go
│
├── api-server/
│   └── main.go
│
├── dns-controller/
│   └── main.go
│
└── certificate-controller/
    └── main.go
```

Each directory has a specific purpose.

## 3.1 `protocol`

The `protocol` package contains the shared data structures.

Controllers and the API server use these structures.

For example:

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

The protocol package must stay small.

It must not contain DNS code.

It must not contain certificate code.

It must not contain controller logic.

The dependency direction is:

```text
protocol
   ^
   |
   +-- api-server
   |
   +-- dns-controller
   |
   +-- certificate-controller
```

This makes the protocol reusable.

---

# 4. Resources

A resource represents desired state.

A resource has five main parts:

```text
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

## 4.1 `apiVersion`

`apiVersion` identifies the API version.

Example:

```json
"apiVersion": "v1"
```

Use the version to allow the resource definition to evolve.

Do not use the version to identify the controller.

The controller identifies resources by their `kind`.

---

## 4.2 `kind`

`kind` identifies the type of resource.

Examples:

```json
"kind": "DNSRecord"
```

and:

```json
"kind": "Certificate"
```

A controller normally watches one or more kinds.

For example:

```text
DNSRecord
    |
    +-- DNS controller

Certificate
    |
    +-- Certificate controller
```

---

## 4.3 `metadata`

Metadata identifies a resource.

Example:

```json
"metadata": {
  "name": "example",
  "namespace": "default"
}
```

The complete metadata structure is:

```go
type Metadata struct {
    Name            string            `json:"name"`
    Namespace       string            `json:"namespace,omitempty"`
    UID             string            `json:"uid,omitempty"`
    Generation      uint64            `json:"generation,omitempty"`
    ResourceVersion uint64            `json:"resourceVersion,omitempty"`
    Labels          map[string]string `json:"labels,omitempty"`
    Annotations     map[string]string `json:"annotations,omitempty"`
}
```

### `name`

The resource name.

Example:

```json
"name": "example"
```

The name identifies the resource together with its API version, kind, and namespace.

### `namespace`

The namespace is optional.

Example:

```json
"namespace": "default"
```

A resource can therefore be identified by:

```text
apiVersion
kind
namespace
name
```

### `uid`

The API server creates the UID when a resource is created.

A controller must not normally create the UID.

Example:

```json
"uid": "7f3f0a7e-..."
```

### `generation`

The generation identifies changes to the desired resource configuration.

The API server starts the generation at `1`.

When the desired `spec` changes, the API server increments the generation.

A controller can use the generation to determine which desired configuration it has processed.

### `resourceVersion`

The resource version changes when the resource changes.

It is managed by the API server.

It is useful for observing resource changes and for future concurrency control.

### `labels`

Labels are key/value metadata.

Example:

```json
"labels": {
  "environment": "production",
  "owner": "network"
}
```

### `annotations`

Annotations are also key/value metadata.

They are intended for additional information.

Example:

```json
"annotations": {
  "description": "Main DNS record"
}
```

---

# 5. `spec`

`spec` contains the desired state.

The API server does not interpret the contents of `spec`.

For example:

```json
"spec": {
  "hostname": "example.test",
  "address": "192.168.1.10"
}
```

The API server stores this data.

The DNS controller interprets it.

This is an important design rule.

The API server is generic.

A controller owns the meaning of its resource.

For a DNS controller:

```text
spec.hostname
spec.address
```

may define a DNS record.

For a certificate controller:

```text
spec.hostname
spec.issuer
```

may define a certificate request.

The API server does not need to know this.

---

# 6. `status`

`status` represents observed state.

Example:

```json
"status": {
  "ready": true
}
```

The current example controllers only log reconciliation activity. They do not yet update resource status.

A future controller can use status to report information such as:

```text
ready
error
message
observedGeneration
certificate expiry
DNS propagation state
```

The important distinction is:

```text
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

# 7. Resource Kinds

The API server supports dynamic resource-kind registration.

A resource kind is described by:

```go
type ResourceKind struct {
    APIVersion string `json:"apiVersion"`
    Kind       string `json:"kind"`
    Resource   string `json:"resource"`
    Namespaced bool   `json:"namespaced"`
}
```

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

The controller registers its resource kind when it starts.

This means the API server does not need to contain a hard-coded list such as:

```go
if kind == "DNSRecord" {
    ...
}

if kind == "Certificate" {
    ...
}
```

Do not add controller-specific logic to the API server.

Instead, a new controller registers its own resource kind.

---

# 8. API Server

The API server is the central process.

The current server listens on:

```text
http://localhost:8080
```

The API server provides four main functions:

```text
1. Resource kind discovery
2. Resource CRUD
3. Resource listing
4. Resource watching
```

CRUD means:

```text
Create
Read
Update
Delete
```

The API server currently stores data in memory.

If the API server stops, the resources are lost.

This is intentional in the current simple implementation.

A persistent database can be added later.

---

# 9. API Server Store

The API server has an internal store.

Conceptually, the store contains:

```go
type Store struct {
    mu       sync.RWMutex
    revision uint64
    watchID  uint64
    items    map[string]protocol.Resource
    kinds    map[string]protocol.ResourceKind
    watchers map[uint64]*Watcher
}
```

The store has three important maps.

## 9.1 Resources

```text
items
```

contains the resources.

The resource key contains information such as:

```text
apiVersion
kind
namespace
name
```

This allows resources with the same name to exist in different namespaces.

---

## 9.2 Kinds

```text
kinds
```

contains registered resource kinds.

For example:

```text
v1/DNSRecord
v1/Certificate
```

---

## 9.3 Watchers

```text
watchers
```

contains clients that currently watch resources.

When a resource changes, the API server sends an event to matching watchers.

---

# 10. Concurrency

The API server can handle multiple HTTP requests at the same time.

Go's HTTP server starts request handling concurrently.

This means two requests can access the store at the same time.

The store therefore uses:

```go
sync.RWMutex
```

The mutex protects the internal maps.

Read operations use a read lock.

Write operations use a write lock.

For example:

```text
GET resource
    |
    +-- read lock

POST resource
    |
    +-- write lock
```

Do not remove the mutex when changing the store.

Without synchronization, concurrent requests can corrupt the store or cause data races.

---

# 11. API Endpoint Summary

The current API endpoints are:

| Method | Endpoint                | Purpose               |
| ------ | ----------------------- | --------------------- |
| GET    | `/api/v1/kinds`         | List registered kinds |
| POST   | `/api/v1/kinds`         | Register a kind       |
| GET    | `/api/v1/resources`     | List resources        |
| GET    | `/api/v1/watch`         | Watch resources       |
| GET    | `/api/v1/{kind}`        | List a kind           |
| POST   | `/api/v1/{kind}`        | Create a resource     |
| GET    | `/api/v1/{kind}/{name}` | Get a resource        |
| PUT    | `/api/v1/{kind}/{name}` | Update a resource     |
| DELETE | `/api/v1/{kind}/{name}` | Delete a resource     |

The following sections describe each endpoint.

---

# 12. List Resource Kinds

Endpoint:

```text
GET /api/v1/kinds
```

This returns all registered resource kinds.

Example:

```bash
curl http://localhost:8080/api/v1/kinds
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

The response contains:

```text
apiVersion
kind
items
```

`items` contains the registered kinds.

---

# 13. Register a Resource Kind

Endpoint:

```text
POST /api/v1/kinds
```

The request body must contain:

```text
apiVersion
kind
resource
namespaced
```

Example:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/kinds \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "resource": "dnsrecords",
    "namespaced": true
  }'
```

The server returns HTTP `201 Created`.

Example response:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true
}
```

The current implementation stores the registration in memory.

If the API server restarts, the registration is lost.

A controller therefore registers its kind when it starts.

---

# 14. List All Resources

Endpoint:

```text
GET /api/v1/resources
```

Example:

```bash
curl http://localhost:8080/api/v1/resources
```

The response is a list of resources.

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

This endpoint is useful for administration and debugging.

---

# 15. Filter Resources by Kind

Use the `kind` query argument.

Endpoint:

```text
GET /api/v1/resources?kind=DNSRecord
```

Example:

```bash
curl 'http://localhost:8080/api/v1/resources?kind=DNSRecord'
```

Only `DNSRecord` resources are returned.

---

# 16. Filter Resources by API Version

Use:

```text
apiVersion
```

Example:

```bash
curl 'http://localhost:8080/api/v1/resources?apiVersion=v1'
```

This returns resources with:

```json
"apiVersion": "v1"
```

---

# 17. Filter Resources by Namespace

Use:

```text
namespace
```

Example:

```bash
curl 'http://localhost:8080/api/v1/resources?namespace=default'
```

This returns resources in the `default` namespace.

---

# 18. Combine Resource Filters

The filters can be combined.

Example:

```bash
curl \
  'http://localhost:8080/api/v1/resources?apiVersion=v1&kind=DNSRecord&namespace=default'
```

This requests:

```text
apiVersion = v1
kind       = DNSRecord
namespace  = default
```

The filters are applied together.

---

# 19. List Resources of a Specific Kind

Endpoint:

```text
GET /api/v1/{kind}
```

For DNS records:

```bash
curl http://localhost:8080/api/v1/DNSRecord
```

For certificates:

```bash
curl http://localhost:8080/api/v1/Certificate
```

The response is a list.

Example:

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

# 20. List a Kind in a Namespace

The collection endpoint accepts the `namespace` query argument.

Example:

```bash
curl \
  'http://localhost:8080/api/v1/DNSRecord?namespace=default'
```

This returns only `DNSRecord` resources in the `default` namespace.

---

# 21. Create a Resource

Endpoint:

```text
POST /api/v1/{kind}
```

Example:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/DNSRecord \
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

The API server creates the resource.

The server generates:

```text
UID
Generation
ResourceVersion
```

The first generation is:

```text
1
```

The server returns HTTP:

```text
201 Created
```

The created resource is returned in the response.

---

# 22. Create a Certificate

Example:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/Certificate \
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

The certificate controller can then observe the new resource.

---

# 23. Read One Resource

Endpoint:

```text
GET /api/v1/{kind}/{name}
```

Example:

```bash
curl http://localhost:8080/api/v1/DNSRecord/example
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

For a namespaced resource, you can specify the namespace:

```bash
curl \
  'http://localhost:8080/api/v1/DNSRecord/example?namespace=default'
```

---

# 24. Update a Resource

Endpoint:

```text
PUT /api/v1/{kind}/{name}
```

Example:

```bash
curl \
  -X PUT \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/DNSRecord/example \
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

If the resource exists, the server updates it.

If the `spec` changes, the generation increases.

For example:

```text
generation 1
       |
       | spec changed
       v
generation 2
```

The resource version also changes.

The API server sends a `MODIFIED` watch event.

---

# 25. Delete a Resource

Endpoint:

```text
DELETE /api/v1/{kind}/{name}
```

Example:

```bash
curl \
  -X DELETE \
  http://localhost:8080/api/v1/DNSRecord/example
```

For a namespaced resource:

```bash
curl \
  -X DELETE \
  'http://localhost:8080/api/v1/DNSRecord/example?namespace=default'
```

If the resource exists, it is removed.

The API server sends a `DELETED` watch event.

This event is important.

A controller must use the delete event to remove the corresponding external state.

For example:

```text
DNSRecord deleted
       |
       v
DNS controller receives DELETED
       |
       v
DNS record removed from DNS server
```

---

# 26. Watch Resources

The watch API is:

```text
GET /api/v1/watch
```

A watch connection remains open.

The API server sends events when resources change.

Example:

```bash
curl -N http://localhost:8080/api/v1/watch
```

The `-N` option tells curl not to buffer the response.

Without `-N`, events may not appear immediately.

---

# 27. Watch a Specific Kind

Example:

```bash
curl -N \
  'http://localhost:8080/api/v1/watch?kind=DNSRecord'
```

This watches only:

```text
DNSRecord
```

resources.

---

# 28. Watch by API Version

Example:

```bash
curl -N \
  'http://localhost:8080/api/v1/watch?apiVersion=v1'
```

This watches resources with API version `v1`.

---

# 29. Watch by Namespace

Example:

```bash
curl -N \
  'http://localhost:8080/api/v1/watch?namespace=default'
```

This watches resources in the `default` namespace.

---

# 30. Combine Watch Filters

Example:

```bash
curl -N \
  'http://localhost:8080/api/v1/watch?apiVersion=v1&kind=DNSRecord&namespace=default'
```

This watches:

```text
apiVersion = v1
kind       = DNSRecord
namespace  = default
```

---

# 31. Watch Events

The watch API uses newline-delimited JSON.

This format is also called NDJSON.

Each line is one JSON object.

Example:

```json
{"type":"ADDED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"},"spec":{"hostname":"example.test","address":"192.168.1.10"}}}
```

A second event can appear on the next line:

```json
{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"DNSRecord","metadata":{"name":"example"},"spec":{"hostname":"example.test","address":"192.168.1.20"}}}
```

A delete event looks like:

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

```text
ADDED
MODIFIED
DELETED
```

---

# 32. Initial Watch State

When a controller starts watching, the API server sends the current matching resources as `ADDED` events.

For example, assume the API server already contains:

```text
DNSRecord/example
DNSRecord/test
```

A new DNS controller starts.

The watch sends:

```text
ADDED DNSRecord/example
ADDED DNSRecord/test
```

The controller reconciles both resources.

After that, the connection remains open.

If a new resource is created:

```text
ADDED DNSRecord/new
```

is sent.

If an existing resource changes:

```text
MODIFIED DNSRecord/example
```

is sent.

If a resource is deleted:

```text
DELETED DNSRecord/test
```

is sent.

This initial state is important for controller recovery.

---

# 33. Controller Architecture

A controller is a separate Go program.

Its job is simple:

```text
Watch resources
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

The controller must not depend on another controller being alive.

For example:

```text
DNS controller
    |
    +-- API server
    |
    +-- DNS system
```

The DNS controller does not call the certificate controller.

The certificate controller does not call the DNS controller.

If controllers need to exchange information, they should use resources.

---

# 34. The Reconciliation Model

A controller should be level-triggered.

This means the controller should not depend only on the exact event that caused a change.

Instead, it should inspect the current desired state and make the external system match it.

For example:

```text
Desired:

example.test -> 192.168.1.10
```

The DNS controller checks the DNS system.

If the DNS system already contains:

```text
example.test -> 192.168.1.10
```

nothing needs to be done.

If it contains:

```text
example.test -> 192.168.1.20
```

the controller changes it.

If it does not contain the record, the controller creates it.

This makes reconciliation idempotent.

---

# 35. Idempotence

A reconciliation function should be safe to run more than once.

For example:

```text
reconcile DNSRecord/example
reconcile DNSRecord/example
reconcile DNSRecord/example
```

should produce the same final state as running it once.

Do not write controllers that assume:

```text
one event = one required action
```

Events can be repeated.

Controllers can restart.

Connections can fail.

A controller must be able to reconcile the same resource again.

---

# 36. Controller Restart

A controller can terminate at any time.

The API server continues to store resources.

For example:

```text
API server
    |
    +-- DNSRecord/example
    +-- DNSRecord/test
```

The DNS controller stops.

The resources remain in the API server.

When the DNS controller starts again, it creates a new watch.

The API server sends the current resources as `ADDED` events.

The controller reconciles them.

This allows the controller to recover without a special recovery protocol.

The controller does not need to know what events occurred while it was offline.

It only needs to know the current desired state.

---

# 37. Watch Reconnection

The current controllers use a simple reconnect loop.

Conceptually:

```go
for {
    if err := watch(); err != nil {
        log.Printf("watch failed: %v", err)
        time.Sleep(2 * time.Second)
    }
}
```

If the API server connection closes:

```text
watch connection closes
        |
        v
watch() returns an error
        |
        v
controller waits
        |
        v
controller connects again
```

The new watch sends the current resources again.

This is another reason why reconciliation must be idempotent.

---

# 38. DNS Controller

The DNS controller registers:

```json
{
  "apiVersion": "v1",
  "kind": "DNSRecord",
  "resource": "dnsrecords",
  "namespaced": true
}
```

It watches:

```text
/api/v1/watch?apiVersion=v1&kind=DNSRecord
```

When it receives an event, it calls its reconciliation logic.

The current example contains:

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

The actual DNS implementation is intentionally not included.

A real controller can use this function to update a DNS server.

---

# 39. Certificate Controller

The certificate controller uses the same architecture.

It registers:

```json
{
  "apiVersion": "v1",
  "kind": "Certificate",
  "resource": "certificates",
  "namespaced": true
}
```

It watches:

```text
/api/v1/watch?apiVersion=v1&kind=Certificate
```

It receives:

```text
ADDED
MODIFIED
DELETED
```

events.

For `ADDED` and `MODIFIED`, it reconciles the requested certificate.

For `DELETED`, it can remove or revoke the corresponding external state, depending on the certificate system.

---

# 40. How to Create a New Controller

You can create a new controller without changing the API server.

For example, assume you want a controller that manages DHCP reservations.

Create:

```text
dhcp-controller/
└── main.go
```

Choose a resource kind:

```text
DHCPReservation
```

Choose its resource name:

```text
dhcReservations
```

A clearer resource name would normally be:

```text
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

# 41. Define the Resource

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

```text
macAddress
address
hostname
```

The API server only stores the JSON.

---

# 42. Register the New Kind

The controller should register the kind when it starts.

The Go structure is:

```go
var dhcpReservationKind = protocol.ResourceKind{
    APIVersion: "v1",
    Kind:       "DHCPReservation",
    Resource:   "dhcpreservations",
    Namespaced: true,
}
```

Send it to:

```text
POST /api/v1/kinds
```

Example code:

```go
func registerKind() error {
    body, err := json.Marshal(dhcpReservationKind)
    if err != nil {
        return err
    }

    response, err := http.Post(
        apiServerURL+"/api/v1/kinds",
        "application/json",
        bytes.NewReader(body),
    )
    if err != nil {
        return err
    }

    defer response.Body.Close()

    if response.StatusCode != http.StatusCreated {
        responseBody, _ := io.ReadAll(response.Body)

        return fmt.Errorf(
            "kind registration returned HTTP %d: %s",
            response.StatusCode,
            string(responseBody),
        )
    }

    return nil
}
```

---

# 43. Watch the New Resource

Build the watch URL:

```go
const watchURL =
    apiServerURL +
    "/api/v1/watch?apiVersion=v1&kind=DHCPReservation"
```

Open the connection:

```go
response, err := http.Get(watchURL)
if err != nil {
    return err
}

defer response.Body.Close()
```

Check the response:

```go
if response.StatusCode != http.StatusOK {
    body, _ := io.ReadAll(response.Body)

    return fmt.Errorf(
        "watch returned HTTP %d: %s",
        response.StatusCode,
        string(body),
    )
}
```

---

# 44. Read Watch Events

Watch events are newline-delimited JSON.

The controller can use `bufio.Scanner`:

```go
scanner := bufio.NewScanner(response.Body)

for scanner.Scan() {
    var event protocol.WatchEvent

    if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
        log.Printf("invalid watch event: %v", err)
        continue
    }

    if err := reconcile(event); err != nil {
        log.Printf("reconcile failed: %v", err)
    }
}
```

The scanner reads one event at a time.

The JSON decoder converts the event into:

```go
protocol.WatchEvent
```

---

# 45. Implement Reconciliation

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

# 46. Handle Deletion

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

# 47. Add the Controller to the Taskfile

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

# 48. The Default Task

The default Taskfile task lists available tasks.

Run:

```bash
task
```

This is equivalent to:

```bash
task --list
```

The output shows the available tasks.

This makes the project easier for new users.

---

# 49. Building the Project

Build everything:

```bash
task build
```

The binaries are written to:

```text
bin/
```

For example:

```text
bin/
├── api-server
├── dns-controller
└── certificate-controller
```

After adding a DHCP controller:

```text
bin/
├── api-server
├── dns-controller
├── certificate-controller
└── dhcp-controller
```

---

# 50. Running the API Server

Run:

```bash
task run
```

The API server listens on:

```text
localhost:8080
```

The root path is not an API endpoint.

Therefore:

```bash
curl http://localhost:8080/
```

returns:

```text
404 page not found
```

This is expected.

Use an API endpoint instead:

```bash
curl http://localhost:8080/api/v1/kinds
```

---

# 51. Running a Controller

Run the DNS controller:

```bash
task run-dns
```

Run the certificate controller:

```bash
task run-certificate
```

A controller first registers its resource kind.

It then starts its watch.

A normal controller log looks similar to:

```text
DNS controller started
registered resource kind v1/DNSRecord
watch connected
```

---

# 52. Test the API Manually

Start the API server.

Then register a DNS kind:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/kinds \
  -d '{
    "apiVersion": "v1",
    "kind": "DNSRecord",
    "resource": "dnsrecords",
    "namespaced": true
  }'
```

Check the registered kinds:

```bash
curl http://localhost:8080/api/v1/kinds
```

Create a resource:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/DNSRecord \
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

Read it:

```bash
curl \
  'http://localhost:8080/api/v1/DNSRecord/example?namespace=default'
```

Update it:

```bash
curl \
  -X PUT \
  -H 'Content-Type: application/json' \
  http://localhost:8080/api/v1/DNSRecord/example \
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
curl \
  -X DELETE \
  'http://localhost:8080/api/v1/DNSRecord/example?namespace=default'
```

---

# 53. HTTP Status Codes

The current API uses normal HTTP status codes.

| Status | Meaning                        |
| ------ | ------------------------------ |
| 200    | Request completed successfully |
| 201    | Resource or kind created       |
| 400    | Invalid request                |
| 404    | Resource or endpoint not found |
| 405    | HTTP method is not supported   |
| 409    | Resource already exists        |
| 500    | Internal server error          |

For example, creating a resource with a name that already exists returns:

```text
409 Conflict
```

---

# 54. API Request Flow

A normal resource creation follows this flow:

```text
curl
 |
 | POST /api/v1/DNSRecord
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
 +-- generate UID
 |
 +-- set generation
 |
 +-- assign resourceVersion
 |
 +-- store resource
 |
 +-- notify watchers
 |
 v
HTTP response
```

At the same time, a DNS controller watching the resource receives:

```text
ADDED
```

and starts reconciliation.

---

# 55. API Update Flow

An update follows this flow:

```text
PUT /api/v1/DNSRecord/example
          |
          v
     API server
          |
          v
      Store.Update
          |
          +-- preserve UID
          |
          +-- compare desired state
          |
          +-- increment generation if spec changed
          |
          +-- increment resourceVersion
          |
          +-- send MODIFIED
          |
          v
       HTTP 200
```

The controller receives the `MODIFIED` event.

It then reconciles the new desired state.

---

# 56. API Delete Flow

A delete follows this flow:

```text
DELETE
  |
  v
API server
  |
  v
Store.Delete
  |
  +-- remove resource
  |
  +-- increment revision
  |
  +-- send DELETED
  |
  v
HTTP response
```

The controller receives the deleted resource.

It can then remove external state.

---

# 57. API Server Does Not Run Controllers

The API server does not contain code such as:

```go
runDNSController()
```

or:

```go
runCertificateController()
```

This is intentional.

The API server should remain generic.

This allows a new controller to be added without changing the API server.

For example:

```text
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

# 58. Controllers Communicate Through Resources

Suppose a certificate controller creates a resource that another controller needs.

The controllers should not call each other directly.

Instead:

```text
Certificate Controller
        |
        v
     Resource
        |
        v
Other Controller
```

This keeps the controllers independent.

It also means that one controller can be replaced without changing another controller.

---

# 59. Example Controller Design

A controller normally has these components:

```text
main
 |
 +-- register kind
 |
 +-- watch
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

```text
dns-controller/
├── main.go
├── controller.go
├── reconcile.go
├── client.go
└── types.go
```

Start with one file for a small controller.

Split the code when it becomes difficult to understand.

---

# 60. Validate Resource Data

The API server currently stores `spec` as generic JSON.

This means the API server does not know that:

```json
"address": "192.168.1.10"
```

must be an IP address.

The controller must validate its own resource data.

For example:

```go
hostname, ok := resource.Spec["hostname"].(string)

if !ok || hostname == "" {
    return fmt.Errorf("hostname is required")
}
```

Do not assume that user input is valid.

A controller should validate:

```text
required fields
data types
allowed values
formats
relationships
external constraints
```

---

# 61. Controller Error Handling

A reconciliation error should normally not terminate the controller.

For example:

```go
if err := reconcile(event); err != nil {
    log.Printf("reconcile failed: %v", err)
}
```

The controller remains alive.

A later event can cause another reconciliation.

A production controller should normally add retry and backoff behavior.

For example:

```text
reconcile
   |
   +-- error
       |
       v
    wait
       |
       v
    retry
```

The current example uses a simple reconnect delay for watch failures.

It does not yet implement a complete reconciliation retry queue.

---

# 62. Logging

Controllers should log enough information to identify a resource.

A useful log entry includes:

```text
kind
name
namespace
generation
resourceVersion
```

For example:

```text
RECONCILE DNSRecord/example generation=2 resourceVersion=7
```

This makes troubleshooting easier.

Avoid logging sensitive information.

---

# 63. Resource Version and Generation

These two fields have different purposes.

`generation` represents the desired configuration version.

Example:

```text
generation 1
generation 2
generation 3
```

It changes when the desired `spec` changes.

`resourceVersion` identifies a store revision.

It can change when the resource is updated.

The distinction is:

```text
generation
    |
    +-- desired configuration changed

resourceVersion
    |
    +-- stored resource changed
```

A controller can use `generation` when it wants to know whether it has processed the latest desired configuration.

---

# 64. Current Storage Limitations

The current API server stores all data in memory.

For example:

```text
API server starts
     |
     v
empty store
```

You create:

```text
DNSRecord/example
```

The resource exists.

If the API server stops:

```text
API server stops
     |
     v
memory is lost
```

When it starts again:

```text
empty store
```

This is suitable for development and for understanding the architecture.

It is not suitable for a system that must retain state across API server restarts.

A future implementation can use SQLite or another durable store.

---

# 65. Current Watch Limitations

The current watch implementation is intentionally simple.

A watch connection receives:

```text
current resources
+
future changes
```

It does not provide a historical event log.

For example, if a controller is offline while these events occur:

```text
CREATE A
UPDATE A
DELETE A
```

the controller does not receive those historical events when it reconnects.

Instead, it receives the current state.

This works well with a level-triggered reconciliation model.

However, if a future application requires guaranteed event history, the API server needs a durable event mechanism.

---

# 66. Current API Server Persistence Model

The current architecture is:

```text
HTTP
 |
 v
API server
 |
 v
in-memory store
 |
 +-- resources
 +-- kinds
 +-- watchers
```

A future persistent architecture could be:

```text
HTTP
 |
 v
API server
 |
 v
persistent store
 |
 +-- resources
 +-- kinds
```

The controller API does not need to change significantly.

This is one advantage of keeping persistence behind the API server.

---

# 67. Security Considerations

The current API server is intended for development.

It does not provide production-grade security features such as:

```text
TLS
authentication
authorization
audit logging
network access control
```

Do not expose the development API server directly to an untrusted network.

A production implementation should define:

```text
Who can create resources?
Who can update resources?
Who can delete resources?
Who can watch resources?
Who can register resource kinds?
```

Authentication and authorization should be added before exposing the API to untrusted clients.

---
## Persistent Storage

The API server uses a persistent storage layer to keep resource state across API-server restarts.

The storage layer is intentionally separated from the API server. The API server does not contain database-specific logic. It uses a common `ResourceStore` interface, and different storage implementations can provide the actual persistence mechanism.

The current implementation supports:

* SQLite
* PostgreSQL

The default storage backend is SQLite.

### Storage Architecture

The API server uses the following architecture:

```text
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

The API server is responsible for the HTTP API, resource validation, and watch connections.

The storage implementation is responsible for persistent resource state.

Controllers do not access the database directly. Controllers communicate only with the API server.

```text
Controller
    |
    | HTTP
    v
API Server
    |
    | ResourceStore
    v
Database
```

This separation is important because a controller must not depend on the storage implementation.

A DNS controller can therefore work with SQLite, PostgreSQL, or a future storage implementation without any changes to the controller.

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

The storage layer does not need to understand the contents of `spec` or `status`.

For example, these resources can all be stored using the same mechanism:

```text
DNSRecord
Certificate
TerraformStack
DHCPReservation
PKIResource
```

The API server treats their resource-specific data as opaque JSON.

The controller that owns a resource kind is responsible for understanding the contents of its `spec` and `status`.

### Resource Identity

A resource is uniquely identified by:

```text
apiVersion
kind
namespace
name
```

For example:

```text
v1 / DNSRecord / default / example
```

This allows different resource kinds to use the same name without conflict.

For example:

```text
v1 / DNSRecord / default / example
v1 / Certificate / default / example
```

are different resources.

Each resource also receives a unique `uid` when it is created.

The `uid` remains stable for the lifetime of the resource.

### Resource Version

Every resource receives a monotonically increasing `resourceVersion`.

For example:

```text
resourceVersion = 1
resourceVersion = 2
resourceVersion = 3
resourceVersion = 4
```

The resource version changes whenever a resource is created or updated.

The version is generated by the persistent storage layer.

This is important because controllers use resource versions to reason about changes to resources.

The resource version is also persisted. It is therefore not reset when the API server restarts.

### Generation

Each resource also has a `generation`.

The generation starts at `1` when the resource is created.

When the desired resource configuration changes, the generation is incremented.

For example:

```text
Create:
generation = 1

Update:
generation = 2

Update:
generation = 3
```

Controllers can use the generation to determine whether they have reconciled the current desired configuration.

The storage layer does not need to understand what the generation means. It only maintains the value.

### Resource Kinds

Resource kinds are also persisted.

Controllers register their resource kinds through:

```text
POST /api/v1/kinds
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

The API server stores this registration.

This means that kind registration survives an API-server restart when persistent storage is enabled.

The list of registered kinds can be retrieved with:

```text
GET /api/v1/kinds
```

This allows clients to discover which resource types are available without the API server having a hard-coded list of controllers.

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

```text
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

```text
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

```text
resource_kinds
------------------------------------------------
api_version
kind
resource
namespaced
```

A metadata table stores the current resource-version counter:

```text
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

The DNS controller reads this resource and makes the external DNS system match the desired state.

The controller does not need a private database containing a second copy of the DNS record.

The same principle applies to other controllers.

For example:

```text
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

The API server stores the desired `TerraformStack` resource and its status.

Terraform remains responsible for its own Terraform state.

This prevents multiple systems from becoming competing sources of truth.

### Persistence and Controller Restarts

Persistent storage also changes what happens when a controller stops.

Suppose a user creates a resource while the controller is not running:

```text
User
 |
 | POST DNSRecord
 v
API Server
 |
 v
Database
```

The resource is stored even though the DNS controller is offline.

Later, the controller starts:

```text
DNS Controller
      |
      | LIST / WATCH
      v
API Server
      |
      v
Database
```

The controller receives the existing resource and reconciles it.

This is a key property of the architecture.

Controllers are workers that reconcile persistent desired state. They are not the owners of that state.

### API Server Restart

The API server can also restart without losing resources.

Before the restart:

```text
Database
    |
    +-- DNSRecord/example
    +-- Certificate/example
    +-- TerraformStack/network
```

The API server stops.

The database remains available.

After the API server starts:

```text
API Server
    |
    v
Database
    |
    +-- DNSRecord/example
    +-- Certificate/example
    +-- TerraformStack/network
```

The resources are still present.

Controllers can reconnect and reconcile the current state.

### Watch State

Resource state is persistent, but active watch connections are not.

Watch connections exist only in API-server memory:

```text
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

When the API server stops, the watch connections are lost.

Controllers are expected to reconnect.

When a controller reconnects, it can obtain the current resource state and continue reconciliation.

This follows the level-triggered controller model. Controllers should not depend on receiving every historical event in order to recover.

### Storage Abstraction

The storage interface is defined independently of the database implementation.

Conceptually:

```go
type ResourceStore interface {
    Create(...)
    Get(...)
    List(...)
    Update(...)
    Delete(...)

    RegisterKind(...)
    ListKinds(...)
}
```

The API server depends on this interface rather than directly depending on SQLite or PostgreSQL.

This makes it possible to add another storage implementation later without changing the API layer.

For example:

```text
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

The database is the source of truth.

A resource operation follows this general sequence:

```text
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
API Server
  |
  | watch event
  v
Controllers
```

The important ordering is that the persistent state is updated before the corresponding watch event is published.

A watch event is therefore a notification that persistent state changed. It is not the persistent state itself.

If a controller misses a watch event, it can query the API server and obtain the current state.

### Future Storage Improvements

The current implementation provides persistent resource storage and an in-memory watch mechanism.

Possible future improvements include:

* PostgreSQL `LISTEN/NOTIFY` for API-server instances that share one PostgreSQL database
* Persistent event history
* Resource-version based watch recovery
* Optimistic concurrency using resource versions
* Database connection pooling configuration
* Automatic database migrations
* Database backup and restore tooling
* High-availability API-server deployments

These improvements do not require changing the resource model or controller API.

The central design remains:

```text
                Persistent Desired State
                         |
                         v
                    API Server
                         |
                 +-------+-------+
                 |               |
              LIST/GET        WATCH
                 |               |
                 v               v
             Controllers    Controllers
                 |
                 v
          External Systems
```

The database provides durable state. The API server provides the resource API. Controllers provide reconciliation logic. Each component has a separate responsibility.

---

# 68. Production Improvements

The current implementation is a small control-plane foundation.

A production system can add:

```text
authentication
authorization
TLS
resource schemas
resource validation
optimistic concurrency
durable watch history
watch resource versions
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

These features should be added only when they are needed.

Keep the generic core small.

---

# 69. Testing Controllers

A controller should be tested independently.

The project includes integration testing through:

```bash
task test-controllers
```

The test process can:

```text
create resources
update resources
delete resources
verify controller activity
clean up resources
```

A controller test should verify behavior rather than only verify that an HTTP request returned `200`.

For example:

```text
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

# 70. Recommended Controller Development Process

Use this sequence when creating a new controller.

## Step 1: Define the resource

Decide:

```text
kind
apiVersion
spec
status
```

Keep the specification small.

## Step 2: Register the kind

Add:

```text
POST /api/v1/kinds
```

to the controller startup process.

## Step 3: Create the watch

Watch only the resource kinds that the controller needs.

## Step 4: Implement reconciliation

Handle:

```text
ADDED
MODIFIED
DELETED
```

## Step 5: Make reconciliation idempotent

Running reconciliation more than once must be safe.

## Step 6: Validate the resource

Check all required fields.

## Step 7: Handle external failures

Do not terminate the controller because an external system is temporarily unavailable.

## Step 8: Add tests

Test create, update, delete, restart, and error conditions.

## Step 9: Add logging

Include resource identity in log messages.

## Step 10: Add the controller to the build

Update the Taskfile.

---

# 71. Complete Controller Pattern

A small controller can follow this pattern:

```go
package main

import (
    "bufio"
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "time"

    "controlplane/protocol"
)

const apiServerURL = "http://localhost:8080"

var resourceKind = protocol.ResourceKind{
    APIVersion: "v1",
    Kind:       "ExampleResource",
    Resource:   "exampleresources",
    Namespaced: true,
}

func main() {
    log.Println("controller started")

    if err := registerKind(); err != nil {
        log.Fatalf("failed to register resource kind: %v", err)
    }

    for {
        if err := watch(); err != nil {
            log.Printf("watch failed: %v", err)
            log.Println("reconnecting in 2 seconds")
            time.Sleep(2 * time.Second)
        }
    }
}

func registerKind() error {
    body, err := json.Marshal(resourceKind)
    if err != nil {
        return err
    }

    response, err := http.Post(
        apiServerURL+"/api/v1/kinds",
        "application/json",
        bytes.NewReader(body),
    )
    if err != nil {
        return err
    }

    defer response.Body.Close()

    if response.StatusCode != http.StatusCreated {
        responseBody, _ := io.ReadAll(response.Body)

        return fmt.Errorf(
            "kind registration returned HTTP %d: %s",
            response.StatusCode,
            string(responseBody),
        )
    }

    return nil
}

func watch() error {
    watchURL := apiServerURL +
        "/api/v1/watch?apiVersion=v1&kind=ExampleResource"

    response, err := http.Get(watchURL)
    if err != nil {
        return err
    }

    defer response.Body.Close()

    if response.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(response.Body)

        return fmt.Errorf(
            "watch returned HTTP %d: %s",
            response.StatusCode,
            string(body),
        )
    }

    log.Println("watch connected")

    scanner := bufio.NewScanner(response.Body)

    for scanner.Scan() {
        var event protocol.WatchEvent

        if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
            log.Printf("invalid watch event: %v", err)
            continue
        }

        if err := reconcile(event); err != nil {
            log.Printf("reconcile failed: %v", err)
        }
    }

    return scanner.Err()
}

func reconcile(event protocol.WatchEvent) error {
    switch event.Type {
    case protocol.Added:
        return reconcileResource(event.Object)

    case protocol.Modified:
        return reconcileResource(event.Object)

    case protocol.Deleted:
        return deleteResource(event.Object)

    default:
        return fmt.Errorf(
            "unknown event type %q",
            event.Type,
        )
    }
}

func reconcileResource(
    resource protocol.Resource,
) error {
    log.Printf(
        "reconciling %s/%s",
        resource.Kind,
        resource.Metadata.Name,
    )

    // Make the external system match resource.Spec.

    return nil
}

func deleteResource(
    resource protocol.Resource,
) error {
    log.Printf(
        "deleting external state for %s/%s",
        resource.Kind,
        resource.Metadata.Name,
    )

    // Remove external state.

    return nil
}
```

This pattern is enough to build a basic controller.

---

# 72. Common Mistakes

## Mistake 1: Put controller logic in the API server

Do not do this:

```go
if resource.Kind == "DNSRecord" {
    updateDNS()
}
```

The API server must remain generic.

---

## Mistake 2: Depend on event history

Do not assume that every event will always be delivered.

A controller must be able to reconstruct the desired state from the current resource.

---

## Mistake 3: Make reconciliation non-idempotent

Avoid code that creates duplicate external objects every time it receives an event.

Instead, first determine the actual external state.

Then change it only when necessary.

---

## Mistake 4: Ignore delete events

If the controller creates external state, it normally must also remove that state when the resource is deleted.

---

## Mistake 5: Stop the controller on one external error

External systems can fail temporarily.

A controller should normally retry.

---

## Mistake 6: Assume `spec` is valid

The API server stores generic JSON.

The controller must validate its own resource specification.

---

## Mistake 7: Store controller state only in memory

A controller may restart.

Do not depend on controller memory for the desired state.

The API server resource should contain the desired state.

---

# 73. Recommended Mental Model

When developing a controller, think about the system this way:

```text
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

The controller asks:

```text
What should exist?
```

from the resource.

Then it asks:

```text
What exists now?
```

from the external system.

Then it changes the external system until:

```text
desired state == actual state
```

This is reconciliation.

---

# 74. Complete API Reference

The following is the complete API reference for the current implementation.

## Kind discovery

```text
GET /api/v1/kinds
```

Arguments:

```text
none
```

Returns all registered resource kinds.

---

## Kind registration

```text
POST /api/v1/kinds
```

Request body:

```json
{
  "apiVersion": "v1",
  "kind": "ExampleResource",
  "resource": "exampleresources",
  "namespaced": true
}
```

---

## All resources

```text
GET /api/v1/resources
```

Optional query arguments:

```text
apiVersion
kind
namespace
```

Example:

```text
/api/v1/resources?apiVersion=v1&kind=DNSRecord&namespace=default
```

---

## All resources watch

```text
GET /api/v1/watch
```

Optional query arguments:

```text
apiVersion
kind
namespace
```

Example:

```text
/api/v1/watch?apiVersion=v1&kind=DNSRecord&namespace=default
```

---

## Resource collection

```text
GET /api/v1/{kind}
```

Optional query argument:

```text
namespace
```

Example:

```text
GET /api/v1/DNSRecord?namespace=default
```

---

## Create resource

```text
POST /api/v1/{kind}
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

```text
GET /api/v1/{kind}/{name}
```

Optional query argument:

```text
namespace
```

Example:

```text
GET /api/v1/DNSRecord/example?namespace=default
```

---

## Update resource

```text
PUT /api/v1/{kind}/{name}
```

Request body contains the complete resource.

Example:

```text
PUT /api/v1/DNSRecord/example
```

---

## Delete resource

```text
DELETE /api/v1/{kind}/{name}
```

Optional query argument:

```text
namespace
```

Example:

```text
DELETE /api/v1/DNSRecord/example?namespace=default
```

---

# 75. Final Architecture

The complete current architecture can be represented as:

```text
                         +----------------------+
                         |      API Server      |
                         |                      |
                         | REST API             |
                         | Resource Store       |
                         | Kind Registry        |
                         | Watch Manager        |
                         +----------+-----------+
                                    |
              +---------------------+---------------------+
              |                     |                     |
              v                     v                     v
        DNS Controller       Certificate Controller   Other Controllers
              |                     |                     |
              v                     v                     v
         DNS system             PKI / ACME           External systems
```

The important dependency direction is:

```text
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

The API server does not depend on any specific controller.

A controller depends on the API protocol.

This allows the system to grow without turning the API server into a large collection of application-specific logic.

---

# 76. Summary

Controlplane is a generic resource-oriented control plane.

The API server provides:

```text
resource storage
resource CRUD
resource listing
resource discovery
resource watching
```

The API server does not know what individual resources mean.

Controllers provide the application-specific behavior.

A controller:

```text
registers a resource kind
watches resources
receives events
reconciles desired state
updates an external system
handles deletion
reconnects after failures
```

The most important design rule is:

```text
API server = generic state management

Controller = domain-specific reconciliation
```

When you add a new infrastructure feature, prefer creating a new resource kind and a new controller.

For example:

```text
DNSRecord
    -> DNS controller

Certificate
    -> Certificate controller

DHCPReservation
    -> DHCP controller

FirewallRule
    -> Firewall controller

LoadBalancer
    -> Load balancer controller
```

This keeps the system modular.

A controller can be stopped and restarted.

The desired state remains in the API server.

When the controller starts again, it reads the current state through the watch API and reconciles it.

This makes controllers independent, replaceable, and easier to test.

The current implementation is intentionally small. It provides the core architecture without adding persistence, authentication, authorization, durable event history, or other production features.

Those features can be added later without changing the fundamental controller model.

The core rule remains:

```text
Resources describe desired state.

The API server stores desired state.

Controllers reconcile desired state with actual state.
```

That is the foundation of the Controlplane architecture.
