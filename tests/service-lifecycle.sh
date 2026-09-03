#!/bin/sh
# Real systemd user-service lifecycle exercise for Cortex.
#
# Exercises the full service contract against a genuine systemd user manager:
#   install -> daemon reload -> enable/start -> active-state verification
#   -> status (health) -> stop -> start -> restart
#   -> injected start-limit failure -> reinstall recovery
#   -> uninstall -> fresh reinstall -> final uninstall
#
# The generated unit is additionally validated with `systemd-analyze --user
# verify` while it is installed, so a malformed directive fails this script
# rather than the user's next `cortex service install`.
#
# When no usable user manager is running (for example on a CI runner without a
# login session) an isolated user systemd instance is booted so the exercise
# still runs against real systemd code.
set -eu

die() { echo "service-lifecycle: $*" >&2; exit 1; }

# The ambient user manager and cortex must agree on the user config directory.
# Unset any sandbox override so the unit lands in the real user location
# (~/.config/systemd/user) that the manager searches.
unset XDG_CONFIG_HOME 2>/dev/null || true

BIN=${CORTEX_LIFECYCLE_BIN:-"$HOME/.local/bin/cortex"}
PORT=${CORTEX_LIFECYCLE_PORT:-$(( 10000 + ($$ % 20000) ))}
UNIT="$HOME/.config/systemd/user/cortex.service"

[ -x "$BIN" ] || die "cortex binary not found at $BIN"

ROOT=$(mktemp -d /tmp/cortex-lc-root.XXXXXX)
DATA=$(mktemp -d /tmp/cortex-lc-data.XXXXXX)
CRASH=$(mktemp /tmp/cortex-lc-crash.XXXXXX.sh)
cat > "$CRASH" <<'SH'
#!/bin/sh
exit 1
SH
chmod +x "$CRASH"
cp "$BIN" "$BIN.save"
overwrite_bin() { # replace $BIN atomically, retrying past a transient ETXTBSY
  for _i in $(seq 1 30); do
    cp "$CRASH" "$BIN" 2>/dev/null && return 0
    sleep 0.2
  done
  die "cannot replace the installed binary at $BIN (still executing?)"
}
restore_bin() {
  for _i in $(seq 1 30); do
    cp "$BIN.save" "$BIN" 2>/dev/null && return 0
    sleep 0.2
  done
  die "cannot restore the installed binary at $BIN"
}
cleanup() {
  restore_bin 2>/dev/null || true
  rm -f "$BIN.save" "$CRASH"
  rm -rf "$ROOT" "$DATA"
  systemctl --user stop cortex.service >/dev/null 2>&1 || true
  systemctl --user disable cortex.service >/dev/null 2>&1 || true
  rm -f "$UNIT"
  systemctl --user daemon-reload >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Boot an isolated user manager when the ambient one is not usable. A freshly
# booted instance reports "starting" until it is ready.
if ! systemctl --user is-system-running >/dev/null 2>&1 && [ "${CORTEX_LIFECYCLE_USER_MGR:-0}" != "1" ]; then
  echo "service-lifecycle: booting an isolated user systemd manager"
  export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-$HOME/.cortex-lifecycle-runtime}"
  mkdir -p "$XDG_RUNTIME_DIR"
  chmod 700 "$XDG_RUNTIME_DIR"
  export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
  dbus-daemon --session --fork --address="$DBUS_SESSION_BUS_ADDRESS" --print-address=1 >/dev/null 2>&1 || die "cannot start session dbus"
  systemd --user >/dev/null 2>&1 &
  for _i in $(seq 1 50); do
    systemctl --user is-system-running >/dev/null 2>&1 && break
    sleep 0.2
  done
  systemctl --user is-system-running >/dev/null 2>&1 || die "isolated user manager did not become ready"
  export CORTEX_LIFECYCLE_USER_MGR=1
fi

wait_active() {
  for _i in $(seq 1 60); do
    case "$(systemctl --user is-active cortex.service 2>/dev/null)" in
      active) return 0 ;;
      failed) die "cortex.service entered failed state during $1" ;;
    esac
    sleep 0.5
  done
  die "cortex.service did not become active during $1"
}

install() { "$BIN" service install --root "$ROOT" --data "$DATA" --port "$PORT"; }

echo "== install =="
install
wait_active "install"

echo "== daemon reload =="
systemctl --user daemon-reload

echo "== enablement =="
[ "$(systemctl --user is-enabled cortex.service)" = "enabled" ] || die "cortex.service not enabled"

echo "== systemd-analyze --user verify the generated unit =="
[ -f "$UNIT" ] || die "generated unit not found at $UNIT"
systemd-analyze --user verify "$UNIT"

echo "== active-state verification =="
[ "$(systemctl --user is-active cortex.service)" = "active" ] || die "cortex.service not active"

echo "== status (with health check) =="
"$BIN" service status >/dev/null

echo "== stop / start / restart =="
"$BIN" service stop
[ "$(systemctl --user is-active cortex.service)" = "inactive" ] || die "cortex.service not stopped"
"$BIN" service start
wait_active "start"
"$BIN" service restart
wait_active "restart"

echo "== injected start-limit failure and reinstall recovery =="
"$BIN" service stop >/dev/null
overwrite_bin
for _i in $(seq 1 12); do
  systemctl --user restart cortex.service >/dev/null 2>&1 || true
done
state=$(systemctl --user is-active cortex.service 2>/dev/null || true)
echo "post-crash-loop state: $state"
[ "$state" = "failed" ] || die "expected start-limit 'failed' state after crash loop, got '$state'"
restore_bin
# Reinstall over the failed unit must clear the start-limit state (reset-failed)
# and bring the service back to active.
install >/dev/null
wait_active "reinstall recovery"
[ "$(systemctl --user is-active cortex.service)" = "active" ] || die "reinstall did not recover the service"

echo "== uninstall =="
"$BIN" service uninstall >/dev/null
[ -e "$UNIT" ] && die "unit still present after uninstall"
systemctl --user is-active cortex.service >/dev/null 2>&1 && die "cortex.service still loaded after uninstall"

echo "== reinstall after uninstall (retryable fresh state) =="
install >/dev/null
wait_active "fresh reinstall"
"$BIN" service uninstall >/dev/null

echo "service-lifecycle: ok"