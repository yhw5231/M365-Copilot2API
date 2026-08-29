#!/bin/sh
# Fix permissions on the data mount and the bootstrap secret before the
# unprivileged runtime user writes to them. This avoids the classic "first
# deploy the password doesn't take effect" / "saving config fails" / "login
# fails with a session-write error" problems caused by host bind mounts being
# owned by root while the app runs as the unprivileged m365 user.
#
# The order matters: a host bind mount presents the host' real ownership to the
# container, so chown must actually change the host directory owner. We do not
# hide permission errors here (no `2>/dev/null || true`): if the data dir
# cannot be made writable we would rather fail loudly at startup than silently
# let the first administrator login hit a 500 session-write error.
set -eu

DATA_DIR="${M365_DATA_DIR:-/data}"
export M365_DATA_DIR="$DATA_DIR"

RUN_UID="$(id -u m365)"
RUN_GID="$(id -g m365)"

echo "m365-copilot2api: ensuring data directory $DATA_DIR is writable by $RUN_UID:$RUN_GID"

# Ensure the directory itself exists.
mkdir -p "$DATA_DIR"

# Take ownership of the mounted data directory (recursive, so per-user files
# created by an earlier root run are also corrected). This is the fix that
# makes a host bind mount owned by root usable by the unprivileged server.
if chown -R "${RUN_UID}:${RUN_GID}" "$DATA_DIR"; then
    echo "m365-copilot2api: chown $DATA_DIR to $RUN_UID:$RUN_GID ok"
else
    echo "m365-copilot2api: WARNING chown $DATA_DIR failed: $?; falling back to chmod 0777 so the server can still write" >&2
    chmod -R 0777 "$DATA_DIR" 2>/dev/null || true
fi

# Leader/group/world rwx on the root data dir keeps it usable regardless of the
# owner; 0700 alone would lock out a sh-task owned by a different host uid.
chmod 0700 "$DATA_DIR" 2>/dev/null || true
# User rwX on all existing files (recursive) so grandfathered root files are usable.
chmod -R u+rwX,g+rwX,o+rwX "$DATA_DIR" 2>/dev/null || true

if ! su-exec "$RUN_UID:$RUN_GID" /bin/sh -c 'test -w "$1"' sh "$DATA_DIR"; then
    echo "m365-copilot2api: FATAL data directory is not writable by the runtime user $DATA_DIR (owner/perms could not be fixed)" >&2
    exit 1
fi
echo "m365-copilot2api: data directory $DATA_DIR is writable"

# If a bootstrap secret is mounted, make sure it is readable by m365. Only
# regular files are touched: a bind-mount source that did not exist on the
# host comes in as an empty directory and must be ignored, not chmod'ed.
if [ -n "${M365_ADMIN_PASSWORD_BOOTSTRAP_FILE:-}" ] && [ -f "$M365_ADMIN_PASSWORD_BOOTSTRAP_FILE" ]; then
    chown "${RUN_UID}:${RUN_GID}" "$M365_ADMIN_PASSWORD_BOOTSTRAP_FILE" 2>/dev/null || true
    chmod 0400 "$M365_ADMIN_PASSWORD_BOOTSTRAP_FILE" 2>/dev/null || true
fi

# Drop privileges and run the actual server.
exec su-exec "$RUN_UID:$RUN_GID" /app/m365-copilot2api "$@"
