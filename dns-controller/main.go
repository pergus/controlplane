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

const watchURL = apiServerURL + "/api/v1/watch?apiVersion=v1&kind=DNSRecord"

var dnsRecordKind = protocol.ResourceKind{
	APIVersion: "v1",
	Kind:       "DNSRecord",
	Resource:   "dnsrecords",
	Namespaced: true,
}

func main() {
	log.Println("DNS controller started")

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
	body, err := json.Marshal(dnsRecordKind)
	if err != nil {
		return err
	}

	response, err := http.Post(apiServerURL+"/api/v1/kinds", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		responseBody, _ := io.ReadAll(response.Body)
		return fmt.Errorf("kind registration returned HTTP %d: %s", response.StatusCode, string(responseBody))
	}

	log.Printf("registered resource kind %s/%s", dnsRecordKind.APIVersion, dnsRecordKind.Kind)

	return nil
}

func watch() error {
	response, err := http.Get(watchURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return &watchError{status: response.StatusCode, body: string(body)}
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
	log.Printf(
		"RECONCILE DNSRecord/%s generation=%d resourceVersion=%d hostname=%v address=%v",
		resource.Metadata.Name,
		resource.Metadata.Generation,
		resource.Metadata.ResourceVersion,
		resource.Spec["hostname"],
		resource.Spec["address"],
	)

	// The real DNS implementation would reconcile the desired
	// DNS state against the actual DNS provider/server here.
	//
	// This operation should be idempotent.

	return nil
}

func removeDNSRecord(resource protocol.Resource) error {
	log.Printf(
		"DELETE DNSRecord/%s generation=%d resourceVersion=%d",
		resource.Metadata.Name,
		resource.Metadata.Generation,
		resource.Metadata.ResourceVersion,
	)

	// Remove the DNS record from the external system here.
	//
	// This operation should also be idempotent.

	return nil
}

type watchError struct {
	status int
	body   string
}

func (e *watchError) Error() string {
	return "watch returned HTTP " + http.StatusText(e.status) + ": " + e.body
}
