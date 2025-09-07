#!/bin/bash

# generate a private key
openssl genrsa -out server.key 2048

# generate a certificate signing request
openssl req -new -key server.key -out server.csr \
    -subj "/C=US/ST=State/L=City/O=Organization/CN=localhost"

# generate a self-signed certificate valid for 365 days
openssl x509 -req -days 365 -in server.csr -signkey server.key -out server.crt

# remove CSR
rm server.csr

echo "Generated server.crt and server.key for testing inbound TLS connections"
echo ""
echo "1. Run this proxy: go run main.go"
echo "2. Connect with psql using SSL: psql 'host=localhost port=5434 user=app_user dbname=postgres sslmode=require'"
