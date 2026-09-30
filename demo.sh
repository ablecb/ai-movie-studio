#!/usr/bin/env bash
# 一键演示：故障演练 → 完整运行 → 状态 → 经验库
set -e
cd "$(dirname "$0")"
export PATH="$HOME/tools/go/bin:$PATH"

if [ ! -x bin/studio ]; then
  echo "== 编译 =="
  go build -o bin/studio ./cmd/studio
fi

BRIEF="一部 3 分钟科幻短片：深空信使收到来自地球麦田的神秘信号，踏上归途。"

echo; echo "== 1/4 故障演练（自我修复演示） =="
./bin/studio run --drill --brief "$BRIEF" | sed -n '1,14p'

echo; echo "== 2/4 状态 =="
RUN=$(ls -d runs/run-* | tail -1)
./bin/studio status --run "$RUN"

echo; echo "== 3/4 产物清单 =="
ls "$RUN/artifacts" "$RUN/output"

echo; echo "== 4/4 经验记忆库（自我成长演示） =="
./bin/studio lessons

echo; echo "完整产物见: $RUN/"
