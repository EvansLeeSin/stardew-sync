#!/system/bin/sh
# Called when the module is removed. Remove all data (config, token, backups).
MODDIR=${0%/*}
pkill -TERM -f "$MODDIR/bin/stardew-sync" 2>/dev/null
rm -rf /data/adb/stardew-sync
rm -rf /data/media/0/Android/data/com.chucklefish.stardewvalley/files/.stardew-sync-tmp
