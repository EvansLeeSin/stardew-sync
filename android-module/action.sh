#!/system/bin/sh
# KernelSU "Action" button: sync now.
MODDIR=${0%/*}
echo "Stardew Sync: 立即同步..."
"$MODDIR/bin/stardew-sync" -data /data/adb/stardew-sync sync
