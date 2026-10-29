#!/usr/bin/env bash
# 把 goacos 的 SDK 复制到某个消费方仓里（仓内副本）。
#
# 为什么需要这个脚本
# ------------------
# goacos 仓里「把跨仓共享的注册策略收进 SDK」的那一版（ConfigFromEnv / Announce，
# 也就是注册时机 / 端口口径 / 地址口径 / 注销时机这五个决定）**不在任何已发布
# tag 里** —— v1.2.0 的 sdk/registry 只有 client.go / discover.go / doc.go。
#
# 消费方要的就是那五个决定。在自己的仓里重写一遍，就是「每个仓各推导一次」
# —— 那正是这个 SDK 要消灭的漂移（acc-go 已经自己写了一遍，见它的
# cmd/acc/coordination.go）。
#
# 代价必须说清楚
# --------------
# 这份副本是**未发布**代码：goacos 发下一个 tag 之前，它只存在于消费方仓里。
#   - 好处：逐字节来自某个 commit（可复现）、离线可构建、漂移可被自动检出；
#   - 代价：别的仓拿不到同一份代码，goacos 的修复不会自动流过去。
# owner 发 tag 之后的收尾（副本目录下的 PROVENANCE.md 里也写了）：
#   删掉 go.mod 里的 replace，跑 go mod vendor，把副本换成 vendor/ 下的发布版。
#
# 用法
# ----
#   scripts/vendor-sdk-copy.sh <消费方仓根> <goacos-ref>
#
# 例：
#   goacos/scripts/vendor-sdk-copy.sh ../memora 4db8a14
#   goacos/scripts/vendor-sdk-copy.sh ../openpocket/backend 4db8a14
#
# ref 必须是**显式**给的（commit 或 tag）。不给就取 HEAD 是错的：那等于让副本
# 在没人注意的时候跟着上游往前走，「这个仓用的是哪一版 SDK」就又不可知了。
#
# 副本落在 <消费方仓根>/third_party/goacos/。

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GOACOS_REPO="$(cd "${HERE}/.." && pwd)"

CONSUMER="${1:-}"
REF="${2:-}"

if [ -z "${CONSUMER}" ] || [ -z "${REF}" ]; then
  echo "用法：scripts/vendor-sdk-copy.sh <消费方仓根> <goacos-ref>" >&2
  echo "ref 必须显式给。不给就取 HEAD，等于让副本悄悄跟着上游走。" >&2
  exit 2
fi

CONSUMER="$(cd "${CONSUMER}" && pwd)"
OUT="${CONSUMER}/third_party/goacos"

if [ ! -f "${CONSUMER}/go.mod" ]; then
  echo "消费方仓根没有 go.mod：${CONSUMER}" >&2
  exit 2
fi

if ! SHA="$(git -C "${GOACOS_REPO}" rev-parse --verify "${REF}^{commit}" 2>/dev/null)"; then
  echo "goacos 仓里没有 ${REF} 这个 commit。" >&2
  exit 2
fi

# 源文件清单从那个 commit 的树里现取，而不是写死在脚本里：
# 上游将来新增 sdk/registry 下的文件时，这个清单会自动跟上，
# 少拷一个文件就会在漂移判据里报出来（而不是编译时才炸）。
UPSTREAM="$(git -C "${GOACOS_REPO}" ls-tree --name-only "${SHA}" sdk/registry/ \
  | sed 's|^sdk/registry/||' \
  | grep -v '_test\.go$')"

if [ -z "${UPSTREAM}" ]; then
  echo "${SHA} 的 sdk/registry/ 下没有任何非测试文件 —— 对照路径错了。" >&2
  exit 2
fi

# 嵌套模块的 module 指令取自上游同一个 commit 的 go.mod。
UPSTREAM_MODULE="$(git -C "${GOACOS_REPO}" show "${SHA}:go.mod" | awk '/^module /{print $2; exit}')"
if [ -z "${UPSTREAM_MODULE}" ]; then
  echo "${SHA} 的 go.mod 里读不到 module 指令。" >&2
  exit 2
fi

# ⚠ 嵌套模块的 **go 指令取消费方的**，不取上游的。
#
# 理由（实测，不是推测）：消费方主模块的 go 指令是 1.27.1，而上游是 1.27.2；
# 本机 toolchain 是 go1.27.1。于是上游那个 1.27.2 会让消费方的构建直接失败：
#   go: module ./dep requires go >= 1.27.2 (running go 1.27.1)
# （GOTOOLCHAIN=auto **不会**因为依赖要求更高版本就自动切换 —— 切换只由主模块的
# go/toolchain 指令驱动。）
#
# 语言版本的补丁位差异（1.27.1 vs 1.27.2）不影响 SDK 代码的语义，
# 而 go.mod 里那个 go 指令属于**这个仓**，不属于上游的 sdk/registry 目录
# ⇒ 它不在「逐字节比对」的范围里。
CONSUMER_GO="$(awk '/^go /{print $2; exit}' "${CONSUMER}/go.mod")"
if [ -z "${CONSUMER_GO}" ]; then
  echo "消费方 go.mod 里读不到 go 指令：${CONSUMER}/go.mod" >&2
  exit 2
fi

UPSTREAM_GO="$(git -C "${GOACOS_REPO}" show "${SHA}:go.mod" | awk '/^go /{print $2; exit}')"
if [ -z "${UPSTREAM_GO}" ]; then
  echo "${SHA} 的 go.mod 里读不到 go 指令。" >&2
  exit 2
fi

rm -rf "${OUT}"
mkdir -p "${OUT}/sdk/registry"

for name in ${UPSTREAM}; do
  git -C "${GOACOS_REPO}" show "${SHA}:sdk/registry/${name}" > "${OUT}/sdk/registry/${name}"
done

cat > "${OUT}/go.mod" <<EOF
module ${UPSTREAM_MODULE}

go ${CONSUMER_GO}
EOF

# ── PROVENANCE ──────────────────────────────────────────────────────────────
# 由脚本现算，不手写：手写的溯源信息总有一天和文件对不上。

sha_of() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

COMMIT_DATE="$(git -C "${GOACOS_REPO}" log -1 --format=%cI "${SHA}")"
COMMIT_SUBJECT="$(git -C "${GOACOS_REPO}" log -1 --format=%s "${SHA}")"

# 「这份代码有没有被发布过」——发布过就不该再用副本，应该改用 go mod vendor。
TAGS_AT_REF="$(git -C "${GOACOS_REPO}" tag --points-at "${SHA}" | tr '\n' ' ' | sed 's/[[:space:]]*$//')"
if [ -n "${TAGS_AT_REF}" ]; then
  PUBLISHED="**已发布**（tag: ${TAGS_AT_REF}）—— 可以改用 \`go mod vendor\` 走发布版。"
else
  NEWEST_TAG="$(git -C "${GOACOS_REPO}" describe --tags --abbrev=0 "${SHA}" 2>/dev/null || echo '（无）')"
  BEHIND="$(git -C "${GOACOS_REPO}" rev-list --count "${NEWEST_TAG}..${SHA}" 2>/dev/null || echo '?')"
  PUBLISHED="**未发布**（该 commit 上没有任何 tag；它比最新 tag \`${NEWEST_TAG}\` 新 ${BEHIND} 个提交）"
fi

{
  echo "# goacos SDK 仓内副本 —— 溯源"
  echo
  echo "本目录是 \`github.com/halfking/goacos\` 的 \`sdk/registry\` 包的**逐字节副本**。"
  echo "它由 \`goacos/scripts/vendor-sdk-copy.sh\` 生成，请勿手工编辑。"
  echo
  echo "## 副本来自哪里"
  echo
  echo "| 项 | 值 |"
  echo "| --- | --- |"
  echo "| goacos 仓 | \`${GOACOS_REPO}\` |"
  echo "| 消费方仓 | \`${CONSUMER}\` |"
  echo "| ref | \`${REF}\` |"
  echo "| commit | \`${SHA}\` |"
  echo "| 提交时间 | ${COMMIT_DATE} |"
  echo "| 提交标题 | ${COMMIT_SUBJECT} |"
  echo "| 发布状态 | ${PUBLISHED} |"
  echo "| 嵌套模块 go 指令 | \`${CONSUMER_GO}\`（取自**消费方** go.mod；上游是 \`${UPSTREAM_GO}\`） |"
  echo
  echo "## 逐文件 sha256"
  echo
  echo "| 文件 | sha256 |"
  echo "| --- | --- |"
  for name in ${UPSTREAM}; do
    echo "| \`sdk/registry/${name}\` | \`$(sha_of "${OUT}/sdk/registry/${name}")\` |"
  done
  echo
  echo "## 重新生成"
  echo
  echo '```bash'
  echo "cd ${GOACOS_REPO} && scripts/vendor-sdk-copy.sh ${CONSUMER} ${SHA}"
  echo '```'
  echo
  echo "ref 必须**显式**给出：默认取 HEAD 会让这份副本在没人注意的时候跟着上游走，"
  echo "「这个仓用的是哪一版 SDK」就又不可知了。"
  echo
  echo "## 为什么是副本而不是 \`go mod vendor\`"
  echo
  echo "\`service.go\`（\`ConfigFromEnv\` / \`Announce\`）**不在任何已发布 tag 里** ——"
  echo "v1.2.0 的 \`sdk/registry\` 只有 client.go / discover.go / doc.go。"
  echo "消费方需要的正是这个 SDK 收进去的那五个决定（注册时机、端口口径、地址口径、"
  echo "注销时机、失败不阻塞服务）；自己重写一遍就是「每个仓各推导一次」，"
  echo "正是这个 SDK 要消灭的漂移。"
  echo
  echo "## 发 tag 之后怎么收尾"
  echo
  echo "1. owner 在 goacos 仓发 tag（包含 \`sdk/registry/service.go\` 的那一版）；"
  echo "2. 消费方 \`go.mod\` 删掉 \`replace github.com/halfking/goacos => ./third_party/goacos\`，"
  echo "   改成 \`require github.com/halfking/goacos <新 tag>\`；"
  echo "3. 跑 \`go mod vendor\`，把 \`vendor/github.com/halfking/goacos\` 提交进仓；"
  echo "4. 删掉本目录，并把漂移判据的对照基准从 commit 换成 tag"
  echo "   （与 acc-go 的 \`test_acc_vendor_matches_tag.py\` 同一形状）。"
  echo
  echo "在第 1 步完成之前，本副本是该消费方唯一的 SDK 来源 —— "
  echo "它不会自动跟着 goacos 仓更新，漂移由 myvpn 的判据盯着。"
} > "${OUT}/PROVENANCE.md"

echo "已生成：${OUT}"
echo "  源：${GOACOS_REPO} @ ${SHA}"
echo "  文件：$(echo ${UPSTREAM} | tr '\n' ' ')"
echo "  嵌套模块 go 指令：${CONSUMER_GO}（上游 ${UPSTREAM_GO}）"
echo "  发布状态：${PUBLISHED}"
