package messaging

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

const (
	RegistrationStreamName = "CONTROLPLANE_KIND_REGISTRATIONS"
	RegistrationSubject    = "controlplane.kinds.register"
	RegistrationConsumer   = "api-server-kind-registrations"
)

func KindEventStreamName(_ string, kind string) string {
	token := kindEventToken(kind)
	return "EVENTS_" + strings.ToUpper(strings.ReplaceAll(token, "%", "_"))
}

func KindEventSubject(_ string, kind string) string {
	return "events." + kindEventToken(kind)
}

func kindEventToken(kind string) string {
	switch kind {
	case "DNSRecord":
		return "dns"
	case "Certificate":
		return "certificate"
	}

	token := strings.ToLower(kind)

	const hexDigits = "0123456789abcdef"
	var encoded strings.Builder
	for index := 0; index < len(token); index++ {
		character := token[index]
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			encoded.WriteByte(character)
			continue
		}
		encoded.WriteByte('%')
		encoded.WriteByte(hexDigits[character>>4])
		encoded.WriteByte(hexDigits[character&0x0f])
	}
	name := encoded.String()
	if name == "" {
		name = "kind"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	hash := sha256.Sum256([]byte(kind))
	return name + "-" + hex.EncodeToString(hash[:4])
}

func encodeKind(apiVersion, kind string) []byte {
	encoded := make([]byte, 8+len(apiVersion)+len(kind))
	binary.BigEndian.PutUint32(encoded, uint32(len(apiVersion)))
	copy(encoded[4:], apiVersion)
	offset := 4 + len(apiVersion)
	binary.BigEndian.PutUint32(encoded[offset:], uint32(len(kind)))
	copy(encoded[offset+4:], kind)
	return encoded
}
