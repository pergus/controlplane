package messaging

import (
	"strings"
	"testing"
)

func TestKindEventStreamIdentityIsStableAndDistinct(t *testing.T) {
	first := KindEventStreamName("v1", "DNSRecord")
	if first != "EVENTS_DNS" {
		t.Fatalf("DNSRecord stream = %q, want EVENTS_DNS", first)
	}
	if first != KindEventStreamName("v1", "DNSRecord") {
		t.Fatal("kind stream name is not stable")
	}
	if first != KindEventStreamName("v2", "DNSRecord") {
		t.Fatal("API versions of one kind should share an event stream")
	}
	if first == KindEventStreamName("v1", "Certificate") {
		t.Fatal("different kinds shared an event stream")
	}
	if stream := KindEventStreamName("v1", "Certificate"); stream != "EVENTS_CERTIFICATE" {
		t.Fatalf("Certificate stream = %q, want EVENTS_CERTIFICATE", stream)
	}
	if KindEventStreamName("v1", "DNS.Group") == KindEventStreamName("v1", "DNS_2EGroup") {
		t.Fatal("different kind names collided after sanitizing stream names")
	}
}

func TestKindEventSubjectsSupportNATSWildcards(t *testing.T) {
	if subject := KindEventSubject("v1", "DNSRecord"); subject != "events.dns" {
		t.Fatalf("DNSRecord subject = %q, want events.dns", subject)
	}
	if subject := KindEventSubject("v1", "Certificate"); subject != "events.certificate" {
		t.Fatalf("Certificate subject = %q, want events.certificate", subject)
	}
	if subject := KindEventSubject("v2", "DNSRecord"); subject != "events.dns" {
		t.Fatalf("DNSRecord v2 subject = %q, want events.dns", subject)
	}
	if subject := KindEventSubject("v1", "DNS.Group"); !strings.HasPrefix(subject, "events.dns%2egroup-") {
		t.Fatalf("subject with separator = %q, want encoded token", subject)
	}
}
