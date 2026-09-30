#!/usr/bin/env bash

set -euo pipefail

API="http://localhost:8080"
KIND="DNSRecord"
COUNT=20

echo "Testing DNS controller with ${COUNT} resources"

for i in $(seq 1 ${COUNT}); do
    NAME="dns-test-${i}"

    curl -s -X POST "${API}/api/v1/${KIND}" \
        -H "Content-Type: application/json" \
        -d "{
            \"apiVersion\": \"v1\",
            \"kind\": \"${KIND}\",
            \"metadata\": {
                \"name\": \"${NAME}\"
            },
            \"spec\": {
                \"hostname\": \"${NAME}.example.internal\",
                \"address\": \"192.168.1.$((100 + i))\"
            }
        }" > /dev/null

    echo "Created ${NAME}"
done

echo "Checking created resources"

for i in $(seq 1 ${COUNT}); do
    NAME="dns-test-${i}"

    RESULT=$(curl -s "${API}/api/v1/${KIND}/${NAME}")

    echo "${RESULT}" | grep -q "${NAME}"

    echo "Verified ${NAME}"
done

echo "Updating resources"

for i in $(seq 1 ${COUNT}); do
    NAME="dns-test-${i}"

    curl -s -X PUT "${API}/api/v1/${KIND}/${NAME}" \
        -H "Content-Type: application/json" \
        -d "{
            \"apiVersion\": \"v1\",
            \"kind\": \"${KIND}\",
            \"metadata\": {
                \"name\": \"${NAME}\"
            },
            \"spec\": {
                \"hostname\": \"${NAME}.example.internal\",
                \"address\": \"10.0.0.$i\"
            }
        }" > /dev/null

    echo "Updated ${NAME}"
done

echo "Deleting resources"

for i in $(seq 1 ${COUNT}); do
    NAME="dns-test-${i}"

    curl -s -X DELETE "${API}/api/v1/${KIND}/${NAME}" > /dev/null

    echo "Deleted ${NAME}"
done

echo "Checking cleanup"

RESULT=$(curl -s "${API}/api/v1/${KIND}")

for i in $(seq 1 ${COUNT}); do
    NAME="dns-test-${i}"

    if echo "${RESULT}" | grep -q "${NAME}"; then
        echo "ERROR: ${NAME} still exists"
        exit 1
    fi
done

echo "DNS controller test completed successfully"