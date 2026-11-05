#!/usr/bin/env bash
# 把 goacos 的 **Node 绑定**复制到某个消费方仓里（仓内副本）。
#
# 与 vendor-sdk-copy.sh（Go 版）的关系
# ------------------------------------
# 同一个目的、同样的纪律，但**复制源不同**，所以分开写：
#
#   · Go 版从**某个 commit** 的树里取文件（sdk/registry/*.go）；
#   · Node 版从**工作区**取文件（sdk/registry-node/*）。
#
# 为什么 Node 版必须从工作区取
# ----------------------------
# 2026-10-29 实测发现 Node 绑定的公开面有三个缺陷（Config 只存在于类型声明里、
# ConfigOptions 用 snake_case 而运行时读 camelCase、Config 类根本不存在），
# 修复就在 goacos 仓的**工作区**里 —— 而我们**不替 owner 提交**。
#
# ⇒ 于是副本的来源必须**如实标注**：base commit 是哪个、每个文件与那个 commit
#   是否一致、哪几个不一致（未提交的上游改动）。
#   一旦 owner 提交了那个修复，重跑本脚本（base commit 换成新 commit）就会得到
#   逐字节一致的副本，PROVENANCE 里的「不一致」那几行也会自动翻成「一致」。
#
# 代价必须说清楚
# --------------
# 这份副本是**未发布**代码：`goacos-registry` 在 npm 上**不存在**（实测 404），
# 在 PyPI 上也不存在（实测 404）。goacos 发版之前，它只存在于消费方仓里。
#
# 用法
# ----
#   scripts/vendor-sdk-node-copy.sh <消费方仓根> <goacos-base-commit>
#
# 例：
#   goacos/scripts/vendor-sdk-node-copy.sh ../marketing 4db8a14
#
# base commit 必须**显式**给（不给就取 HEAD 是错的：那等于让副本在没人注意的
# 时候跟着上游往前走，「这个仓用的是哪一版 SDK」就又不可知了）。
#
# 副本落在 <消费方仓根>/third_party/goacos-registry/。

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GOACOS_REPO="$(cd "${HERE}/.." && pwd)"

CONSUMER="${1:-}"
BASE="${2:-}"

if [ -z "${CONSUMER}" ] || [ -z "${BASE}" ]; then
  echo "用法：scripts/vendor-sdk-node-copy.sh <消费方仓根> <goacos-base-commit>" >&2
  echo "base commit 必须显式给。不给就取 HEAD，等于让副本悄悄跟着上游走。" >&2
  exit 2
fi

CONSUMER="$(cd "${CONSUMER}" && pwd)"
OUT="${CONSUMER}/third_party/goacos-registry"
SRC="${GOACOS_REPO}/sdk/registry-node"

if [ ! -d "${SRC}" ]; then
  echo "goacos 仓里没有 sdk/registry-node/：${SRC}" >&2
  exit 2
fi

if ! BASE_SHA="$(git -C "${GOACOS_REPO}" rev-parse --verify "${BASE}^{commit}" 2>/dev/null)"; then
  echo "goacos 仓里没有 ${BASE} 这个 commit。" >&2
  exit 2
fi

# 源文件清单从目录现取，而不是写死在脚本里：上游新增文件时清单自动跟上，
# 少拷一个文件会在漂移判据里报出来（而不是运行时才炸）。
FILES="$(cd "${SRC}" && ls -1 | grep -v '^test\.js$' | grep -v '\.DS_Store$')"

if [ -z "${FILES}" ]; then
  echo "sdk/registry-node/ 下没有任何可复制的文件 —— 对照路径错了。" >&2
  exit 2
fi

rm -rf "${OUT}"
mkdir -p "${OUT}"

for name in ${FILES}; do
  cp "${SRC}/${name}" "${OUT}/${name}"
done

sha_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

# 每个文件与 base commit 的版本是否一致 —— 这一列是这份 PROVENANCE 的核心。
DIFF_ROWS=""
MISMATCH_COUNT=0
for name in ${FILES}; do
  if git -C "${GOACOS_REPO}" cat-file -e "${BASE_SHA}:sdk/registry-node/${name}" 2>/dev/null; then
    BASE_SHA256="$(git -C "${GOACOS_REPO}" show "${BASE_SHA}:sdk/registry-node/${name}" | shasum -a 256 2>/dev/null | awk '{print $1}')"
    if [ "${BASE_SHA256}" = "$(sha_of "${OUT}/${name}")" ]; then
      DIFF_ROWS="${DIFF_ROWS}| \`${name}\` | 一致 |
"
    else
      DIFF_ROWS="${DIFF_ROWS}| \`${name}\` | **不一致**（工作区里有未提交的上游改动） |
"
      MISMATCH_COUNT=$((MISMATCH_COUNT + 1))
    fi
  else
    DIFF_ROWS="${DIFF_ROWS}| \`${name}\` | base commit 里没有这个文件 |
"
    MISMATCH_COUNT=$((MISMATCH_COUNT + 1))
  fi
done

BASE_DATE="$(git -C "${GOACOS_REPO}" log -1 --format=%cI "${BASE_SHA}")"
BASE_SUBJECT="$(git -C "${GOACOS_REPO}" log -1 --format=%s "${BASE_SHA}")"
TAGS_AT_REF="$(git -C "${GOACOS_REPO}" tag --points-at "${BASE_SHA}" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
if [ -n "${TAGS_AT_REF}" ]; then
  PUBLISHED="**已发布**（tag: ${TAGS_AT_REF}）"
else
  PUBLISHED="**未发布**（该 commit 上没有任何 tag）"
fi

if [ "${MISMATCH_COUNT}" -gt 0 ]; then
  MISMATCH_NOTE="⚠ 本副本含 **${MISMATCH_COUNT} 个文件与 base commit 不一致** —— 那是 goacos 仓**工作区**里未提交的改动（Node 绑定公开面的修复，2026-10-29）。owner 提交之后，重跑本脚本（base commit 换成新 commit）即可得到逐字节一致的副本。"
else
  MISMATCH_NOTE="本副本与 base commit **逐字节一致**。"
fi

{
  echo "# goacos Node 绑定 仓内副本 —— 溯源"
  echo
  echo "本目录是 \`github.com/halfking/goacos\` 的 \`sdk/registry-node\` 的**逐字节副本**。"
  echo "它由 \`goacos/scripts/vendor-sdk-node-copy.sh\` 生成，请勿手工编辑。"
  echo
  echo "## 副本来自哪里"
  echo
  echo "| 项 | 值 |"
  echo "| --- | --- |"
  echo "| goacos 仓 | \`${GOACOS_REPO}\` |"
  echo "| 消费方仓 | \`${CONSUMER}\` |"
  echo "| base commit | \`${BASE}\` |"
  echo "| base commit 全名 | \`${BASE_SHA}\` |"
  echo "| base commit 时间 | ${BASE_DATE} |"
  echo "| base commit 标题 | ${BASE_SUBJECT} |"
  echo "| 发布状态 | ${PUBLISHED}（npm 实测 404 ⇒ \`goacos-registry\` **未发布到 npm**） |"
  echo "| 复制源 | goacos 仓**工作区**的 \`sdk/registry-node/\`（不是 commit 的树） |"
  echo
  echo "## 与 base commit 的差异"
  echo
  echo "| 文件 | 与 base commit |"
  echo "| --- | --- |"
  printf '%s' "${DIFF_ROWS}"
  echo
  echo "${MISMATCH_NOTE}"
  echo
  echo "## 逐文件 sha256"
  echo
  echo "| 文件 | sha256 |"
  echo "| --- | --- |"
  for name in ${FILES}; do
    echo "| \`${name}\` | \`$(sha_of "${OUT}/${name}")\` |"
  done
  echo
  echo "## 重新生成"
  echo
  echo '```bash'
  echo "cd ${GOACOS_REPO} && scripts/vendor-sdk-node-copy.sh ${CONSUMER} ${BASE_SHA}"
  echo '```'
  echo
  echo "base commit 必须**显式**给出：默认取 HEAD 会让这份副本在没人注意的时候跟着上游走，"
  echo "「这个仓用的是哪一版 SDK」就又不可知了。"
  echo
  echo "## 为什么是副本而不是 npm 依赖"
  echo
  echo "\`goacos-registry\` 在 npm 上**不存在**（实测 404）⇒ 没有可安装的发布版。"
  echo "而且 Node 绑定有一个**发布前就该修**的缺陷：\`index.d.ts\` 声明了 \`Config\`"
  echo "而运行时没导出它、\`ConfigOptions\` 用 snake_case 而运行时读 camelCase。"
  echo "等 owner 发版（npm publish）之后，把本目录换成 npm 依赖即可，收尾步骤见下。"
  echo
  echo "## 发版之后怎么收尾"
  echo
  echo "1. owner 在 goacos 仓提交 Node 公开面的修复，并发布 \`goacos-registry\` 到 npm；"
  echo "2. 消费方 \`package.json\` 加依赖 \`\"goacos-registry\": \"^<新版本>\"\`；"
  echo "3. 删掉本目录，import 改成 \`from 'goacos-registry'\`；"
  echo "4. 把漂移判据的对照基准从「工作区副本」换成 npm 发布版。"
  echo
  echo "在第 1 步完成之前，本副本是该消费方唯一的 SDK 来源 ——"
  echo "它不会自动跟着 goacos 仓更新，漂移由 myvpn 的判据盯着。"
} > "${OUT}/PROVENANCE.md"

echo "已生成：${OUT}"
echo "  源：${SRC}（工作区），base commit ${BASE_SHA}"
echo "  文件：$(echo ${FILES} | tr '\n' ' ')"
echo "  与 base commit 不一致的文件数：${MISMATCH_COUNT}"
