#!/usr/bin/env bash
# 把 goacos 的 **Python 绑定**复制到某个消费方仓里（仓内副本）。
#
# 与 vendor-sdk-copy.sh（Go 版）/ vendor-sdk-node-copy.sh（Node 版）的关系
# ---------------------------------------------------------------------------
# 同一个目的、同样的纪律，但**复制源不同**，所以分开写：
#
#   · Go 版   从**某个 commit** 的树里取文件（sdk/registry/*.go）；
#   · Node 版 从**工作区**取文件（sdk/registry-node/*）——
#             因为 Node 绑定的公开面有未提交的修复（2026-10-29 实测）。
#   · Python 版从**某个 commit** 的树里取文件（sdk/registry-python/*）——
#             Python 绑定与 HEAD **逐字节一致**（2026-10-29 实测：
#             `git diff --stat HEAD -- sdk/registry-python/` 为空），
#             所以它**既可以**从 commit 取、**也应该**从 commit 取。
#
# 为什么「也应该」
# --------------
# 从 commit 取 ⇒ 副本的内容只由 **base commit** 这一个参数决定，
# 不受工作区状态影响 ⇒ 同一个 base commit 跑两次得到同一个 sha256。
# 从工作区取（Node 版）时，同一个 base commit 跑两次可能得到不同的副本
# （因为工作区被人改过）⇒ 副本的 sha256 就不再是「可复算」的东西。
#
# ⚠ 但工作区与 base commit 的差异**必须如实报出来**（本脚本的「与 base
#   commit 的差异」一节）：哪天 Python 绑定也有了未提交的修复，
#   副本就不再等于上游工作区 —— 那时要先把上游改动提交，再重跑本脚本。
#
# 代价必须说清楚
# --------------
# 这份副本是**未发布**代码：`goacos-registry` 在 PyPI 上**不存在**
# （2026-10-29 实测 404）⇒ 没有可安装的发布版。
#
# 用法
# ----
#   scripts/vendor-sdk-python-copy.sh <消费方仓根> <goacos-base-commit> [输出子目录]
#
# 例：
#   goacos/scripts/vendor-sdk-python-copy.sh ../stock/smm 4db8a14 src/third_party/goacos_registry
#
# base commit 必须**显式**给（不给就取 HEAD 是错的：那等于让副本在没人注意的
# 时候跟着上游往前走，「这个仓用的是哪一版 SDK」就又不可知了）。
#
# 输出子目录默认 `src/third_party/goacos_registry`。
# 为什么默认落在 `src/` 之下而不是仓根：消费方 **smm** 的 Web 镜像是
# 多阶段构建，运行时阶段只 `COPY src/ ./src/` ⇒ 仓根下的目录**进不了镜像**。
# 落在 `src/` 之下，副本才会跟着代码进容器。
#
# ⚠ 副本目录名里有 `third_party` ⇒ myvpn 的源码接线探针会**跳过**它
#   （探针只把「业务代码自己的 import」算接线，第三方副本不算）。
#   这正是探针 SKIP_DIR_NAMES 里 `third_party` 的用途。

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GOACOS_REPO="$(cd "${HERE}/.." && pwd)"

CONSUMER="${1:-}"
BASE="${2:-}"
OUT_REL="${3:-src/third_party/goacos_registry}"

if [ -z "${CONSUMER}" ] || [ -z "${BASE}" ]; then
  echo "用法：scripts/vendor-sdk-python-copy.sh <消费方仓根> <goacos-base-commit> [输出子目录]" >&2
  echo "base commit 必须显式给。不给就取 HEAD，等于让副本悄悄跟着上游走。" >&2
  exit 2
fi

CONSUMER="$(cd "${CONSUMER}" && pwd)"
OUT="${CONSUMER}/${OUT_REL}"

if [ ! -d "${GOACOS_REPO}/sdk/registry-python" ]; then
  echo "goacos 仓里没有 sdk/registry-python/：${GOACOS_REPO}/sdk/registry-python" >&2
  exit 2
fi

if ! BASE_SHA="$(git -C "${GOACOS_REPO}" rev-parse --verify "${BASE}^{commit}" 2>/dev/null)"; then
  echo "goacos 仓里没有 ${BASE} 这个 commit。" >&2
  exit 2
fi

# 源文件 → 副本内文件名的映射（`副本内文件名|上游相对路径`，一行一个）。
#
# ⚠ 清单是**写死**的，不是从目录现取的 —— 与 Node 版相反，理由：
#   目录里有 `__pycache__/` 与 `.DS_Store`（实测），现取会把垃圾一起拷进来；
#   而「上游新增了文件」这件事必须**由人**决定要不要带进来
#   （新增文件可能引入新依赖，而本 SDK 的卖点是零第三方依赖）。
#   上游新增文件时，本脚本会**报出来**（见「与 base commit 的差异」一节）。
FILES="__init__.py|sdk/registry-python/goacos_registry/__init__.py
LICENSE|sdk/registry-python/LICENSE
README.md|sdk/registry-python/README.md"

sha_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

rm -rf "${OUT}"
mkdir -p "${OUT}"

DIFF_ROWS=""
MISMATCH_COUNT=0
MISSING_COUNT=0
SHA_ROWS=""
dst_list=""

while IFS= read -r line; do
  [ -z "${line}" ] && continue
  dst_name="${line%%|*}"
  src_rel="${line#*|}"
  src_path="${GOACOS_REPO}/${src_rel}"

  if ! git -C "${GOACOS_REPO}" cat-file -e "${BASE_SHA}:${src_rel}" 2>/dev/null; then
    echo "base commit ${BASE_SHA} 里没有 ${src_rel} —— 副本清单与上游对不上，停下来。" >&2
    exit 2
  fi

  # 从 **commit 的树**里取字节（不是工作区）。
  git -C "${GOACOS_REPO}" show "${BASE_SHA}:${src_rel}" > "${OUT}/${dst_name}"

  BASE_SHA256="$(sha_of "${OUT}/${dst_name}")"
  SHA_ROWS="${SHA_ROWS}| \`${dst_name}\` | \`${BASE_SHA256}\` |
"
  dst_list="${dst_list} ${dst_name}"

  # 工作区版本与 base commit 版本是否一致 —— 不一致要说出来。
  if [ -f "${src_path}" ]; then
    WT_SHA256="$(sha_of "${src_path}")"
    if [ "${WT_SHA256}" = "${BASE_SHA256}" ]; then
      DIFF_ROWS="${DIFF_ROWS}| \`${dst_name}\` | 一致 |
"
    else
      DIFF_ROWS="${DIFF_ROWS}| \`${dst_name}\` | **不一致**（goacos 仓工作区里有未提交的改动） |
"
      MISMATCH_COUNT=$((MISMATCH_COUNT + 1))
    fi
  else
    DIFF_ROWS="${DIFF_ROWS}| \`${dst_name}\` | 工作区里没有这个文件 |
"
    MISMATCH_COUNT=$((MISMATCH_COUNT + 1))
  fi
done <<EOF
${FILES}
EOF

# 上游目录里有没有**本脚本没带进来**的运行时文件 —— 上游新增文件必须被人看见。
BASE_TREE_FILES="$(git -C "${GOACOS_REPO}" ls-tree -r --name-only "${BASE_SHA}" sdk/registry-python/goacos_registry | grep -v '/$' || true)"
EXTRA_ROWS=""
for f in ${BASE_TREE_FILES}; do
  base_name="$(basename "${f}")"
  case " ${dst_list} " in
    *" ${base_name} "*) continue ;;
  esac
  EXTRA_ROWS="${EXTRA_ROWS}- \`${f}\`（副本里**没有**它 —— 需要人判断要不要带进来）
"
  MISSING_COUNT=$((MISSING_COUNT + 1))
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
  MISMATCH_NOTE="⚠ 本副本含 **${MISMATCH_COUNT} 个文件与 goacos 仓工作区不一致** —— 那说明上游在 base commit 之后还有未提交的改动。owner 提交之后，重跑本脚本（base commit 换成新 commit）即可得到与工作区逐字节一致的副本。"
else
  MISMATCH_NOTE="本副本与 goacos 仓工作区**逐字节一致**（base commit = 工作区）。"
fi

if [ "${MISSING_COUNT}" -gt 0 ]; then
  EXTRA_NOTE="⚠ 上游的运行时目录里有 ${MISSING_COUNT} 个文件**没有**被本脚本带走 —— 见下表。带不带来要人判断（新增文件可能引入新依赖，而本 SDK 的卖点是零第三方依赖）。"
else
  EXTRA_NOTE="上游运行时目录里的文件**全部**在本副本里（没有漏带）。"
fi

{
  echo "# goacos Python 绑定 仓内副本 —— 溯源"
  echo
  echo "本目录是 \`github.com/halfking/goacos\` 的 \`sdk/registry-python/goacos_registry\` 的**逐字节副本**。"
  echo "它由 \`goacos/scripts/vendor-sdk-python-copy.sh\` 生成，请勿手工编辑。"
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
  echo "| 发布状态 | ${PUBLISHED}（PyPI 实测 404 ⇒ \`goacos-registry\` **未发布到 PyPI**） |"
  echo "| 复制源 | goacos 仓 **base commit 的树**（\`git show ${BASE_SHA}:<path>\`），不是工作区 |"
  echo
  echo "## 与 base commit 的差异"
  echo
  echo "| 文件 | 与 base commit |"
  echo "| --- | --- |"
  printf '%s' "${DIFF_ROWS}"
  echo
  echo "${MISMATCH_NOTE}"
  echo
  echo "## 上游运行时目录里没被带走的文件"
  echo
  if [ -n "${EXTRA_ROWS}" ]; then
    printf '%s' "${EXTRA_ROWS}"
  else
    echo "（无）"
  fi
  echo
  echo "${EXTRA_NOTE}"
  echo
  echo "## 逐文件 sha256"
  echo
  echo "| 文件 | sha256 |"
  echo "| --- | --- |"
  printf '%s' "${SHA_ROWS}"
  echo
  echo "## 重新生成"
  echo
  echo '```bash'
  echo "cd ${GOACOS_REPO} && scripts/vendor-sdk-python-copy.sh ${CONSUMER} ${BASE_SHA} ${OUT_REL}"
  echo '```'
  echo
  echo "base commit 必须**显式**给出：默认取 HEAD 会让这份副本在没人注意的时候跟着上游走，"
  echo "「这个仓用的是哪一版 SDK」就又不可知了。"
  echo
  echo "## 为什么是副本而不是 PyPI 依赖"
  echo
  echo "\`goacos-registry\` 在 PyPI 上**不存在**（实测 404）⇒ 没有可安装的发布版。"
  echo "而且本 SDK 的一个特性是**零第三方依赖**（只用标准库 urllib/threading/json）——"
  echo "这一点在复制成副本之后仍然成立，副本不引入任何新依赖。"
  echo
  echo "## 发版之后怎么收尾"
  echo
  echo "1. owner 在 goacos 仓发 tag（含 \`sdk/registry/service.go\`）并发布 \`goacos-registry\` 到 PyPI；"
  echo "2. 消费方 \`requirements-docker.txt\` 加依赖 \`goacos-registry==<新版本>\`；"
  echo "3. 删掉本目录，import 改成 \`from goacos_registry import ...\`；"
  echo "4. 把漂移判据的对照基准从「仓内副本」换成 PyPI 发布版。"
  echo
  echo "在第 1 步完成之前，本副本是该消费方唯一的 SDK 来源 ——"
  echo "它不会自动跟着 goacos 仓更新，漂移由 myvpn 的判据盯着。"
} > "${OUT}/PROVENANCE.md"

echo "已生成：${OUT}"
echo "  源：${GOACOS_REPO} 的 commit ${BASE_SHA}（commit 树，不是工作区）"
echo "  文件：$(echo ${dst_list})"
echo "  与工作区不一致的文件数：${MISMATCH_COUNT}"
echo "  上游没被带走的文件数：${MISSING_COUNT}"
