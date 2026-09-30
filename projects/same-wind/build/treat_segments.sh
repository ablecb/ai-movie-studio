#!/usr/bin/env bash
# treat_segments.sh - 把 5.13s 原始片段处理成 19.9s 的 photo-roman 镜头
#
# 结构：首帧定格 6s -> 慢速实拍 7.9s (setpts 1.54) -> 末帧定格 6s。
# 与旧版 assemble.ps1 的差别：结尾定格取**运动末帧**而不是首帧——
# 旧版把首帧 PNG 同时用作结尾，动作结束后会"跳回开头"冻结 6 秒，
# 观感是一次明显的倒吸；末帧定格让镜头停在动作结束处，读作收势。
#
# 用法: treat_segments.sh <run_dir> [shot_id ...]
#   run_dir 的 output/shots/ 下需有 <id>.mp4（原始 5.13s）与 <id>.png（首帧）。
#   原始片段备份为 <id>.src.mp4，处理结果写回 <id>.mp4（deliver 直接消费该文件）。
set -euo pipefail

RUN="$(cd "$1" && pwd)" || exit 1
RUN="$(cygpath -m "$RUN" 2>/dev/null || echo "$RUN")"   # ffmpeg 是原生 Windows 构建，要 D:/ 形式
shift || true
SHOTS=("$@")
[ ${#SHOTS[@]} -eq 0 ] && SHOTS=(sw001 sw002 sw003 sw004 sw005 sw006)

FF="C:/Users/Administrator/AppData/Roaming/bilibili/ffmpeg/ffmpeg.exe"
[ -x "$FF" ] || FF=ffmpeg

DIR="$RUN/output/shots"
TMP="$RUN/output/_treat_tmp"
mkdir -p "$TMP"

W=1920; H=1080; FPS=30
HOLD_A=6.0; MOTION=7.9; HOLD_B=6.0
CRF=18
SCALE="scale=${W}:${H}:force_original_aspect_ratio=decrease,pad=${W}:${H}:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=${FPS},format=yuv420p"
SETPPTS="setpts=1.540*PTS"   # 5.13s -> 7.9s

run() { "$FF" -hide_banner -loglevel error -y "$@"; }

for id in "${SHOTS[@]}"; do
  raw="$DIR/$id.mp4"; png="$DIR/$id.png"
  [ -f "$raw" ] || { echo "缺少 $raw" >&2; exit 1; }
  [ -f "$png" ] || { echo "缺少 $png" >&2; exit 1; }

  # 原始素材备份必须**每次刷新**：重渲会写新的 <id>.mp4，
  # 若沿用旧 .src，慢速段会把上一次的运动画面混进这次的定格里
  # （实测出现过"v5 定格 + v2 运动"的拼接混合）。
  cp -f "$raw" "$DIR/$id.src.mp4"

  holdA="$TMP/${id}_holdA.mp4"
  slow="$TMP/${id}_slow.mp4"
  holdBpng="$TMP/${id}_last.png"
  seg="$DIR/$id.mp4.new"

  # 首帧定格 6s
  run -loop 1 -framerate $FPS -i "$png" -t $HOLD_A -vf "$SCALE" \
    -c:v libx264 -preset medium -crf $CRF "$holdA"

  # 慢速实拍 7.9s
  run -i "$DIR/$id.src.mp4" -vf "$SETPPTS,$SCALE" -an \
    -c:v libx264 -preset medium -crf $CRF "$slow"

  # 末帧定格 6s（从慢速片段尾部抽帧）
  run -sseof -0.2 -i "$slow" -frames:v 1 "$holdBpng"
  run -loop 1 -framerate $FPS -i "$holdBpng" -t $HOLD_B -vf "$SCALE" \
    -c:v libx264 -preset medium -crf $CRF "$TMP/${id}_holdB.mp4"

  # 顺序拼接：holdA + slow + holdB = 19.9s（老构建 concat demuxer 要 CRLF）
  printf "file '%s'\r\nfile '%s'\r\nfile '%s'\r\n" \
    "$holdA" "$slow" "$TMP/${id}_holdB.mp4" > "$TMP/$id.list"
  # 踩坑：ffmpeg 3.0.1 的 concat demuxer 在列表文件以正斜杠子目录路径打开时，
  # 会把列表所在目录错误地拼接到条目开头（"…_tmp/D:/…"）导致打不开；
  # 条目是 D:/ 绝对路径时必须用**反斜杠**路径打开列表（与旧 assemble.ps1 一致）。
  list_bs="$(cygpath -w "$TMP/$id.list" 2>/dev/null || echo "$TMP/$id.list")"
  seg_bs="$(cygpath -w "$seg" 2>/dev/null || echo "$seg")"
  run -f concat -safe 0 -i "$list_bs" -c copy -f mp4 "$seg_bs"
  mv -f "$seg" "$DIR/$id.mp4"
  echo "  $id -> 19.9s (末帧定格)"
done

echo "完成：$(ls "$TMP" | wc -l) 个中间文件在 $TMP"
