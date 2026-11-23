#!/usr/bin/env bash
# 提交并推送到 GitHub
#   ./push.sh "提交说明"    → 有改动就提交，然后推 main
#   ./push.sh              → 只推送已有提交
#   ./push.sh --dry-run    → 只验证令牌与连通性
#
# 令牌来源（按优先级）：
#   1. 环境变量 GITHUB_TOKEN
#   2. ~/.workbuddy/.gh_token（本机习惯位置，权限 600）
# 令牌只用在单次 push 的命令行里，不写进 .git/config。
set -euo pipefail

REPO_SLUG="joe12801/meow-ext"
TOKEN_FILE="${HOME}/.workbuddy/.gh_token"

TOKEN="${GITHUB_TOKEN:-}"
if [ -z "$TOKEN" ] && [ -f "$TOKEN_FILE" ]; then
  TOKEN="$(tr -d ' \r\n' < "$TOKEN_FILE")"
fi
[ -n "$TOKEN" ] || { echo "缺少令牌：设 GITHUB_TOKEN，或放一份到 $TOKEN_FILE"; exit 1; }

REMOTE="https://joe12801:${TOKEN}@github.com/${REPO_SLUG}.git"
cd "$(cd "$(dirname "$0")" && pwd)"

if [ "${1:-}" = "--dry-run" ]; then
  echo "--- 仅验证，不推送 ---"
  GIT_TERMINAL_PROMPT=0 git -c credential.interactive=false \
      push --dry-run "$REMOTE" HEAD:main 2>&1 | sed "s/${TOKEN}/<token>/g"
  exit 0
fi

MSG="${1:-chore: update}"
if [ -n "$(git status --porcelain)" ]; then
  echo "--- 本次将提交 ---"
  git status --short
  git add -A
  git -c user.name=joe12801 \
      -c user.email=70465495+joe12801@users.noreply.github.com \
      commit -q -m "$MSG"
else
  echo "--- 工作区干净，只推送已有提交 ---"
fi

echo "--- 推送 ---"
# GIT_TERMINAL_PROMPT=0：无凭据时立即失败，而不是挂住等输入
GIT_TERMINAL_PROMPT=0 git -c credential.interactive=false \
    push "$REMOTE" HEAD:main 2>&1 | sed "s/${TOKEN}/<token>/g"

git fetch -q "$REMOTE" main:refs/remotes/origin/main --force 2>/dev/null || true
echo "--- 对照 ---"
echo "本地 HEAD  : $(git rev-parse --short HEAD)"
echo "origin/main: $(git rev-parse --short origin/main 2>/dev/null || echo 未知)"
