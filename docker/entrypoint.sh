#!/bin/sh
# Fix permissions on the data mount and the bootstrap secret before the
# unprivileged runtime user writes to them. This avoids the classic "first
# deploy the password doesn't take effect" / "saving config fails" problems
# caused by host bind mounts being owned by root while the app runs as m365.
set -eu

RUN_UID="$(id -u m365)"
RUN_GID="$(id -g m365)"

# Ensure the data directory exists and is writable by the runtime user.
mkdir -p "$M365_DATA_DIR"
chown -R "${RUN_UID}:${RUN_GID}" "$M365_DATA_DIR" 2>/dev/null || true
chmod 0700 "$M365_DATA_DIR"

# If a bootstrap secret is mounted, make sure it is readable by m365.
if [ -n "${M365_ADMIN_PASSWORD_BOOTSTRAP_FILE:-}" ] && [ -e "$M365_ADMIN_PASSWORD_BOOTSTRAP_FILE" ]; then
    chown "${RUN_UID}:${RUN_GID}" "$M365_ADMIN_PASSWORD_BOOTSTRAP_FILE" 2>/dev/null || true
    chmod 0400 "$M365_ADMIN_PASSWORD_BOOTSTRAP_FILE" 2>/dev/null || true
fi

# Drop privileges and run the actual server.
exec su-exec "$RUN_UID:$RUN_GID" /app/m365-copilot2api "$@"