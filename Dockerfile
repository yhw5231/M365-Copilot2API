FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X m365-copilot2api/internal/web.Version=${VERSION#v} -X m365-copilot2api/internal/web.Commit=${COMMIT} -X m365-copilot2api/internal/web.BuildTime=${BUILD_TIME}" -o /out/m365-copilot2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache su-exec \
    && addgroup -S m365 && adduser -S -G m365 m365 \
    && mkdir -p /data /app
WORKDIR /app
COPY --from=build /out/m365-copilot2api /app/m365-copilot2api
COPY --from=build /src/web /app/web
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
