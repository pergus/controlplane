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

var certificateKind = protocol.ResourceKind{
	APIVersion: "v1",
	Kind:       "Certificate",
	Resource:   "certificates",
	Namespaced: true,
	Schema: map[string]any{
		"type":     "object",
		"required": []string{"hostname", "issuer"},
		"properties": map[string]any{
			"hostname": map[string]any{"type": "string", "minLength": 1},
			"issuer":   map[string]any{"type": "string", "minLength": 1},
		},
		"additionalProperties": false,
	},
}

func main() {
	log.Println("Certificate controller started")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	broker, err := messaging.Connect(messaging.URLFromEnv(), "certificate-controller")
	if err != nil {
		log.Fatalf("failed to connect to NATS: %v", err)
	}
	defer broker.Close()

	for ctx.Err() == nil {
		if err := broker.RegisterKind(ctx, certificateKind); err != nil {
			log.Printf("kind registration failed: %v", err)
		} else {
			log.Printf("registered resource kind %s/%s", certificateKind.APIVersion, certificateKind.Kind)
			durable := messaging.StableDurableName("certificate-controller", certificateKind.APIVersion, certificateKind.Kind)
			if err := broker.RunKindEvents(ctx, certificateKind.APIVersion, certificateKind.Kind, durable, reconcile); err != nil && ctx.Err() == nil {
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
		return reconcileCertificate(resource)

	case protocol.Modified:
		return reconcileCertificate(resource)

	case protocol.Deleted:
		return removeCertificate(resource)

	default:
		return fmt.Errorf("unknown event type %q", event.Type)
	}
}

func reconcileCertificate(resource protocol.Resource) error {
	log.Printf("RECONCILE Certificate/%s generation=%d resourceVersion=%d hostname=%v issuer=%v", resource.Metadata.Name, resource.Metadata.Generation, resource.Metadata.ResourceVersion, resource.Spec["hostname"], resource.Spec["issuer"])

	// The real certificate implementation would reconcile the
	// desired certificate state against the actual certificate.
	//
	// For example:
	//
	// 1. Read hostname and issuer from resource.Spec.
	// 2. Check whether a valid certificate already exists.
	// 3. Request a certificate if necessary.
	// 4. Renew it when it approaches expiration.
	// 5. Store the certificate and private key.
	// 6. Update resource.Status through the API server.
	//
	// The operation should be idempotent.

	return nil
}

func removeCertificate(resource protocol.Resource) error {
	log.Printf("DELETE Certificate/%s generation=%d resourceVersion=%d", resource.Metadata.Name, resource.Metadata.Generation, resource.Metadata.ResourceVersion)

	// Remove or clean up certificate-related external state here.
	//
	// Cleanup should be idempotent.

	return nil
}
