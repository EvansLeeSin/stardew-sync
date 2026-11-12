#!/system/bin/sh
# daemon.sh start|stop|restart|status
MODDIR=${0%/*}
BIN="$MODDIR/bin/stardew-sync"
DATA=/data/adb/stardew-sync
PIDFILE="$DATA/supervisor.pid"

mkdir -p "$DATA"

is_running() {
  [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

start() {
  if is_running; then echo "already running"; return 0; fi
  # Supervisor loop: restart the daemon if it crashes.
  (
    trap 'kill $CHILD 2>/dev/null; exit 0' TERM INT
    while [ ! -f "$MODDIR/disable" ] && [ ! -f "$MODDIR/remove" ]; do
      "$BIN" -data "$DATA" daemon >>"$DATA/daemon.out" 2>&1 &
      CHILD=$!
      wait $CHILD
      sleep 10
    done
  ) </dev/null >/dev/null 2>&1 &
  echo $! >"$PIDFILE"
  echo "started"
}

stop() {
  if is_running; then
    PID=$(cat "$PIDFILE")
    kill "$PID" 2>/dev/null
    pkill -TERM -f "$BIN -data $DATA daemon" 2>/dev/null
    rm -f "$PIDFILE"
    echo "stopped"
  else
    rm -f "$PIDFILE"
    pkill -TERM -f "$BIN -data $DATA daemon" 2>/dev/null
    echo "not running"
  fi
}

case "$1" in
  start) start ;;
  stop) stop ;;
  restart) stop; sleep 1; start ;;
  status) if is_running; then echo running; else echo stopped; fi ;;
  *) echo "usage: $0 start|stop|restart|status"; exit 2 ;;
esac
