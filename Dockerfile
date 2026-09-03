FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
# docker compose build never passes build-args, which left every local rebuild
# reporting "dev/unknown" in /api/version — deployers could not tell whether the
# container actually carries the new code. When the build context includes the
# git history, derive the values from it; CI passes the explicit ARGs and they
# take precedence over the git-derived defaults.
RUN set -eu; \
    if [ "${VERSION}" = "dev" ] && [ -d .git ]; then \
        apk add --no-cache git >/dev/null 2>&1 || true; \
        VERSION="$(git describe --tags --match 'v[0-9]*' --always 2>/dev/null || echo dev)"; \
    fi; \
    if [ "${COMMIT}" = "unknown" ] && [ -d .git ]; then \
        COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"; \
    fi; \
    if [ "${BUILD_TIME}" = "unknown" ]; then \
        BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
    fi; \
    printf '%s' "${VERSION#v}" > /tmp/version; \
    printf '%s' "${COMMIT}" > /tmp/commit; \
    printf '%s' "${BUILD_TIME}" > /tmp/buildtime; \
    echo "build metadata: version=$(cat /tmp/version) commit=$(cat /tmp/commit) build_time=$(cat /tmp/buildtime)"
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
    -X m365-copilot2api/internal/web.Version=$(cat /tmp/version) \
    -X m365-copilot2api/internal/web.Commit=$(cat /tmp/commit) \
    -X m365-copilot2api/internal/web.BuildTime=$(cat /tmp/buildtime)" \
    -o /out/m365-copilot2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache su-exec tzdata \
    && addgroup -S m365 && adduser -S -G m365 m365 \
    && mkdir -p /data /app
WORKDIR /app
COPY --from=build /out/m365-copilot2api /app/m365-copilot2api
# Entrypoint runs as root: it fixes ownership/permissions of the data and
# secrets mount points (first-deploy issue), then drops to the unprivileged
# m365 user before exec'ing the server.
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
EXPOSE 9090
ENV M365_LISTEN=0.0.0.0:9090 \
    M365_DATA_DIR=/data \
    M365_CONFIG=/data/accounts.json \
    M365_TOKEN_CACHE=/data/token-cache.json \
    M365_SESSION_CACHE=/data/sessions.json \
    M365_CONVERSATION_SESSION_CACHE=/data/conversation-sessions.json \
    M365_API_KEYS=/data/api-keys.json \
    M365_ADMIN_PASSWORD_FILE=/data/admin-password \
    M365_ADMIN_PASSWORD_BOOTSTRAP_FILE=/run/secrets/m365_admin_password
# NOTE: no VOLUME declaration on purpose. An anonymous `/data` volume silently
# replaces host data on every `docker run` recreate; persistence is delegated
# to explicit mounts (docker-compose.yml binds ./data:/data, docker run should
# use `-v m365-data:/data`).
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
