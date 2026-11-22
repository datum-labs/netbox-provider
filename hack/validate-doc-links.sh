#!/usr/bin/env bash
# Checks that relative Markdown links resolve and that text files have no
# trailing whitespace. Uses only Bash and POSIX tools so it runs offline and
# on stock CI runners.
set -euo pipefail
cd "$(dirname "$0")/.."
failed=0
while IFS= read -r file; do
  while IFS= read -r target; do
    target=${target%%#*}
    case "$target" in
      ''|http://*|https://*|mailto:*) continue ;;
    esac
    if [[ ! -e "$(dirname "$file")/$target" ]]; then
      echo "$file: broken local link: $target" >&2
      failed=1
    fi
  done < <(grep -o '\[[^]]*\]([^ )]*' "$file" | sed 's/^.*](//')
done < <(find . -name '*.md' -not -path './.git/*' -not -path './bin/*')
if grep -rnE '[[:blank:]]+$' --include='*.md' --include='*.go' --include='*.yaml' --include='*.yml' --include='*.sh' \
  --exclude-dir=.git --exclude-dir=bin .; then
  echo 'Trailing whitespace found' >&2
  failed=1
fi
exit "$failed"
