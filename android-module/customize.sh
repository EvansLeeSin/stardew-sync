#!/system/bin/sh
# KernelSU / ReSukiSU / Magisk installer hook
SKIPUNZIP=0

if [ "$ARCH" != "arm64" ]; then
  abort "! 仅支持 arm64 设备（当前: $ARCH）"
fi

DATA=/data/adb/stardew-sync
mkdir -p "$DATA"
chmod 700 "$DATA"

set_perm_recursive "$MODPATH" 0 0 0755 0644
set_perm "$MODPATH/bin/stardew-sync" 0 0 0755
set_perm "$MODPATH/service.sh" 0 0 0755
set_perm "$MODPATH/daemon.sh" 0 0 0755
set_perm "$MODPATH/action.sh" 0 0 0755
set_perm "$MODPATH/uninstall.sh" 0 0 0755

ui_print "- 配置/状态/日志目录: $DATA"
if [ ! -f "$DATA/config.json" ]; then
  ui_print "- 首次安装：请在模块 WebUI 中填写服务器地址和 Token"
else
  ui_print "- 已保留现有配置"
fi
ui_print "- 重启后生效（或在 WebUI 中点击“启动守护进程”）"
