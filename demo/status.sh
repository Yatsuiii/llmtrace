#!/bin/bash
# live encode status — run:  watch -n 2 bash /home/Yatsuiii/llmtrace/demo/status.sh
D=/home/Yatsuiii/llmtrace/demo
pid=$(pgrep -xn ffmpeg)
sz=$(stat -c%s "$D/final.mp4" 2>/dev/null || echo 0)
echo "=== $(date +%H:%M:%S) ==="
if [ -z "$pid" ]; then
  if [ "${sz:-0}" -gt 200000 ]; then
    echo "FINISHED  ->  final.mp4 ready (${sz} bytes)"
  else
    echo "no ffmpeg running (not started, or just finished — re-check)"
  fi
  exit 0
fi
est=9200000
pct=$(( sz * 100 / est )); [ "$pct" -gt 99 ] && pct=99
fill=$(( pct / 5 ))
bar=$(printf '%*s' "$fill" '' | tr ' ' '#')$(printf '%*s' "$(( 20 - fill ))" '')
echo "ENCODING  overlays + cards"
echo "[${bar}] ~${pct}%   (final.mp4 ${sz} bytes)"
echo "elapsed:  $(ps -o etime= -p "$pid" | tr -d ' ')"
