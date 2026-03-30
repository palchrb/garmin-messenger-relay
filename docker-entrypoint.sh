#!/bin/sh
set -e

# If the first argument is a known subcommand or flag, prepend the binary name.
# This allows: docker run ... login -config /data/config.yaml
# instead of:  docker run ... garmin-messenger-relay login -config /data/config.yaml
case "$1" in
    run|login|logout|status|test-smtp|init|version|help|-*)
        set -- garmin-messenger-relay "$@"
        ;;
esac

# If running as root (default), fix data directory permissions and drop to
# the unprivileged relay user. This is needed because bind-mounted volumes
# inherit host ownership, which typically doesn't match the relay UID.
if [ "$(id -u)" = "0" ]; then
    chown relay:relay /data
    if [ -d /data/sessions ]; then
        chown -R relay:relay /data/sessions
    fi
    exec su-exec relay "$@"
fi

# If already running as non-root (e.g. --user flag or Kubernetes securityContext),
# skip chown and exec directly.
exec "$@"
