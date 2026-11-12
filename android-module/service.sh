#!/system/bin/sh
# Started by KernelSU after boot. Launches the sync daemon in background.
MODDIR=${0%/*}

until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 5; done
# wait for /data/media to be available (user unlocked)
until [ -d /data/media/0/Android ]; do sleep 5; done

sh "$MODDIR/daemon.sh" start
