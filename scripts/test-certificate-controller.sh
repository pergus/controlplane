#!/usr/bin/env bash

set -euo pipefail

API="http://localhost:8080"
KIND="Certificate"
COUNT=20

echo "Testing certificate controller with ${COUNT} resources"

for i in $(seq 1 "$COUNT"); do
    NAME="certificate-test-${i}"

    curl -fsS -X POST "${API}/api/v1/${KIND}" \
        -H "Content-Type: application/json" \
        -d "{
            \"apiVersion\": \"v1\",
            \"kind\": \"${KIND}\",
            \"metadata\": {
                \"name\": \"${NAME}\"
            },
            \"spec\": {
                \"hostname\": \"${NAME}.example.internal\",
                \"issuer\": \"internal-ca\"
            }
        }" > /dev/null

    echo "Created ${NAME}"
done

echo "Checking created certificates"

for i in $(seq 1 "$COUNT"); do
    NAME="certificate-test-${i}"

    RESULT=$(curl -fsS "${API}/api/v1/${KIND}/${NAME}")

    echo "${RESULT}" | grep -q "${NAME}"

    echo "Verified ${NAME}"
done

echo "Updating certificates"

for i in $(seq 1 "$COUNT"); do
    NAME="certificate-test-${i}"

    curl -fsS -X PUT "${API}/api/v1/${KIND}/${NAME}" \
        -H "Content-Type: application/json" \
        -d "{
            \"apiVersion\": \"v1\",
            \"kind\": \"${KIND}\",
            \"metadata\": {
                \"name\": \"${NAME}\"
            },
            \"spec\": {
                \"hostname\": \"updated-${NAME}.example.internal\",
                \"issuer\": \"internal-ca\"
            }
        }" > /dev/null

    echo "Updated ${NAME}"
done

echo "Deleting certificates"

for i in $(seq 1 "$COUNT"); do
    NAME="certificate-test-${i}"

    curl -fsS -X DELETE "${API}/api/v1/${KIND}/${NAME}" > /dev/null

    echo "Deleted ${NAME}"
done

echo "Checking cleanup"

RESULT=$(curl -fsS "${API}/api/v1/${KIND}")

for i in $(seq 1 "$COUNT"); do
    NAME="certificate-test-${i}"

    if echo "${RESULT}" | grep -q "${NAME}"; then
        echo "ERROR: ${NAME} still exists"
        exit 1
    fi
done

echo "Certificate controller test completed successfully"