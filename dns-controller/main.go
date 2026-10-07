package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"controlplane/messaging"
	"controlplane/protocol"
)

var dnsRecordKind = protocol.ResourceKind{
	APIVersion: "v1",
	Kind:       "DNSRecord",
	Resource:   "dnsrecords",
	Namespaced: true,
	Schema: map[string]any{
		"type":     "object",
		"required": []string{"hostname", "address"},
		"properties": map[string]any{
			"hostname": map[string]any{"type": "string", "minLength": 1},
			"address":  map[string]any{"type": "string", "minLength": 1},
		},
		"additionalProperties": false,
	},
}

func main() {
	log.Println("DNS controller started")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	broker, err := messaging.Connect(messaging.URLFromEnv(), "dns-controller")
	if err != nil {
		log.Fatalf("failed to connect to NATS: %v", err)
	}
	defer broker.Close()

	for ctx.Err() == nil {
		if err := broker.RegisterKind(ctx, dnsRecordKind); err != nil {
			log.Printf("kind registration failed: %v", err)
		} else {
			log.Printf("registered resource kind %s/%s", dnsRecordKind.APIVersion, dnsRecordKind.Kind)
			durable := messaging.StableDurableName("dns-controller", dnsRecordKind.APIVersion, dnsRecordKind.Kind)
			if err := broker.RunKindEvents(ctx, dnsRecordKind.APIVersion, dnsRecordKind.Kind, durable, reconcile); err != nil && ctx.Err() == nil {
				log.Printf("event consumer stopped: %v", err)
			}
		}
		if !waitBeforeRetry(ctx) {
			return
		}
	}
}

func waitBeforeRetry(ctx context.Context) bool {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

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

func reconcileDNSRecord(resource protocol.Resource) error {
	log.Printf("RECONCILE DNSRecord/%s generation=%d resourceVersion=%d hostname=%v address=%v", resource.Metadata.Name, resource.Metadata.Generation, resource.Metadata.ResourceVersion, resource.Spec["hostname"], resource.Spec["address"])

	// The real DNS implementation would reconcile the desired
	// DNS state against the actual DNS provider/server here.
	//
	// This operation should be idempotent.

	return nil
}

func removeDNSRecord(resource protocol.Resource) error {
	log.Printf("DELETE DNSRecord/%s generation=%d resourceVersion=%d", resource.Metadata.Name, resource.Metadata.Generation, resource.Metadata.ResourceVersion)

	// Remove the DNS record from the external system here.
	//
	// This operation should also be idempotent.

	return nil
}
