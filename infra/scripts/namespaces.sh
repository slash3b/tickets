#!/bin/bash
set -e

echo "Deploying monitoring infrastructure to homelab cluster..."

# Ensure we're in the right directory
cd "$(dirname "$0")/.."

echo "Creating namespaces..."
kubectl apply -f cluster-config/namespaces/monitoring.yaml
kubectl apply -f cluster-config/namespaces/logging.yaml

# Wait for namespace to be ready
sleep 2