#!/bin/bash
set -e

# ----------------------------------------------------------------------------------------------
# Cert-Manager (required for OpenTelemetry Operator)
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set installCRDs=true

# ----------------------------------------------------------------------------------------------
# Install OpenTelemetry Collector
helm repo add opentelemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo update
helm upgrade --install otel-collector opentelemetry/opentelemetry-collector \
  --namespace monitoring \
  --set mode=daemonset \
  --set image.repository=otel/opentelemetry-collector-contrib \
  --create-namespace \
  --values otel-values.yaml

# ----------------------------------------------------------------------------------------------
# Add Jaeger repository
helm repo add jaegertracing https://jaegertracing.github.io/helm-charts
helm repo update
helm install jaeger jaegertracing/jaeger \
  --namespace monitoring \
  --create-namespace \
  --values jaeger-values.yaml

# ----------------------------------------------------------------------------------------------
# Add the prometheus-community Helm repository
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install prometheus prometheus-community/prometheus \
  --namespace monitoring \
  --create-namespace \
  --values prometheus-values.yaml

