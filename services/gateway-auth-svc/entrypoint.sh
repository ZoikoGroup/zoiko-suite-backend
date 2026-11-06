#!/bin/sh
# Gateway auth service entrypoint with admin token provisioning

set -e

# Provision admin token file if it doesn't exist (GW-9)
# The volume is mounted at /run/secrets/gateway_admin_token (directory)
TOKEN_FILE="/run/secrets/gateway_admin_token/token"
TOKEN_DIR="/run/secrets/gateway_admin_token"

if [ ! -f "$TOKEN_FILE" ]; then
    echo "Generating gateway admin token..."
    # Generate a random token of at least 24 characters
    TOKEN=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)
    echo "$TOKEN" > "$TOKEN_FILE"
    chmod 0400 "$TOKEN_FILE"
    # Ensure nonroot user can read it
    chown 65532:65532 "$TOKEN_FILE"
    echo "Admin token written to $TOKEN_FILE"
fi

# Export the token file path for the application
export GATEWAY_ADMIN_TOKEN_FILE="$TOKEN_FILE"

# Drop privileges to nonroot user and execute the main binary
exec su-exec nonroot:nonroot /gateway-auth-svc "$@"