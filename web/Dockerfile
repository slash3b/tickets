# syntax=docker/dockerfile:1.9
#
# The web app is NOT built by the shared Go Dockerfile — it is the one thing here
# that is not a Go binary, and pretending otherwise would mean a Dockerfile with
# two unrelated halves.
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /app

# Manifests first so a source edit does not reinstall the dependency tree.
COPY web/package.json web/package-lock.json* ./
# npm ci when a lockfile exists (reproducible), npm install when it does not.
RUN --mount=type=cache,target=/root/.npm \
    if [ -f package-lock.json ]; then npm ci; else npm install; fi

COPY web/ ./
RUN npm run build

FROM nginx:1.27-alpine

COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html

# nginx needs to write its pid file and proxy temp dirs. Granted at build time so
# the running container needs no write access anywhere else.
RUN chown -R nginx:nginx /var/cache/nginx \
 && touch /var/run/nginx.pid && chown nginx:nginx /var/run/nginx.pid

USER nginx
EXPOSE 8080

LABEL org.opencontainers.image.source="https://github.com/slash3b/tickets" \
      org.opencontainers.image.title="tickets-web"
