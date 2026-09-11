#!/usr/bin/env bash
#
# 云端连接服务（cloud-control）部署脚本 —— 在服务器上执行，不在本地跑。
#
# 由 .github/workflows/deploy-cloud-control.yml 通过 ssh 调用，也可人工断网急救时手动跑。
#
# 两种输入方式（二选一）：
#   A) 传源码包，在服务器上编译（推荐，CI 走这条）：
#        SRC_ARCHIVE=/tmp/cloud-control-src.tar.gz bash deploy-cloud-control.sh
#      只传几百 KB 源码，编译交给服务器 —— 跨境链路传 10MB 二进制要十几分钟，
#      传源码只要几秒，而服务器编译这个规模的服务是秒级。
#   B) 使用已编译好的二进制（保留，供本地已构建或应急时使用）：
#        NEW_BIN=/tmp/cloud-control-linux.new bash deploy-cloud-control.sh
#
# 流程：
#   解包 -> 编译(可选) -> ELF 校验 -> 备份旧版 -> docker cp 替换 -> 容器内 sha256 复核
#   -> 重启 -> 健康检查 -> 固化镜像 -> 清理临时产物（只保留最新一份）
#   任一步失败自动回滚到上一个版本。
#
# 磁盘策略（服务器空间紧张，产物只保留最新）：
#   - 源码包、源码目录、临时二进制：用完立即删除；
#   - 回滚二进制固定一个路径，每次覆盖，不累积；
#   - 镜像只保留正在使用的那份，每次部署后清理悬空层。
#   - 仍可用 PRUNE=0 关闭镜像清理。
#
# 可用环境变量覆盖：
#   SRC_ARCHIVE 源码 tar.gz 路径（给了它就在服务器上编译）
#   BUILD_DIR   编译临时目录，默认 /tmp/milevia-cloud-control-build
#   GO_BIN      go 可执行文件，默认 go
#   GOPROXY     Go 模块代理，默认 https://goproxy.cn,direct（国内可达，首次会自动下载 go.mod
#               要求的工具链；之后的工具链与依赖缓存都复用，不再重复下载）
#   CONTAINER   容器名，默认 milevia-cloud-control
#   NEW_BIN     新二进制路径，默认 /tmp/cloud-control-linux.new
#   PREV_BIN    旧二进制备份路径，默认 /tmp/cloud-control-prev
#   TARGET      容器内二进制路径，默认 /usr/local/bin/cloud-control
#   IMAGE       部署后固化的镜像名，默认 milevia-cloud-control:local
#   HEALTH_URL  健康检查地址，默认 http://127.0.0.1:8090/health
#   COMMIT      设为 0 可跳过 docker commit，默认 1
#   PRUNE       设为 0 可跳过镜像悬空层清理，默认 1
#   DRY_RUN     设为 1 只做编译与前置校验并打印计划，不碰容器
#
set -euo pipefail

SRC_ARCHIVE="${SRC_ARCHIVE:-}"
BUILD_DIR="${BUILD_DIR:-/tmp/milevia-cloud-control-build}"
GO_BIN="${GO_BIN:-go}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
CONTAINER="${CONTAINER:-milevia-cloud-control}"
NEW_BIN="${NEW_BIN:-/tmp/cloud-control-linux.new}"
PREV_BIN="${PREV_BIN:-/tmp/cloud-control-prev}"
TARGET="${TARGET:-/usr/local/bin/cloud-control}"
IMAGE="${IMAGE:-milevia-cloud-control:local}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8090/health}"
HEALTH_RETRIES="${HEALTH_RETRIES:-20}"
COMMIT="${COMMIT:-1}"
PRUNE="${PRUNE:-1}"
DRY_RUN="${DRY_RUN:-0}"

log() { printf '[deploy] %s\n' "$*"; }

health_ok() {
  local i code
  for i in $(seq 1 "${HEALTH_RETRIES}"); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "${HEALTH_URL}" || true)"
    if [ "${code}" = "200" ]; then
      log "健康检查通过（第 ${i} 次）：${HEALTH_URL} -> 200"
      return 0
    fi
    sleep 1
  done
  return 1
}

# 把备份的旧二进制放回容器并重启，再确认健康。
restore_previous() {
  if [ ! -f "${PREV_BIN}" ]; then
    log "没有可用的旧版本备份，无法回滚，需人工介入"
    return 1
  fi
  log "回滚到上一个版本"
  docker cp "${PREV_BIN}" "${CONTAINER}:${TARGET}"
  docker restart "${CONTAINER}" >/dev/null
  if health_ok; then
    log "回滚成功"
    return 0
  fi
  log "回滚后仍不健康，需人工介入"
  return 1
}

# ── 前置检查 ────────────────────────────────────────────────
command -v docker >/dev/null || { log "服务器上没有 docker"; exit 1; }
docker inspect "${CONTAINER}" >/dev/null 2>&1 || { log "容器 ${CONTAINER} 不存在"; exit 1; }

if [ -z "${SRC_ARCHIVE}" ] && [ ! -f "${NEW_BIN}" ]; then
  log "既没有源码包（SRC_ARCHIVE=${SRC_ARCHIVE}）也没有现成二进制（${NEW_BIN}），无可部署内容"
  exit 1
fi

# ── 可选：在服务器上编译 ────────────────────────────────────
if [ -n "${SRC_ARCHIVE}" ]; then
  [ -f "${SRC_ARCHIVE}" ] || { log "找不到源码包：${SRC_ARCHIVE}"; exit 1; }
  command -v "${GO_BIN}" >/dev/null || { log "服务器上没有 go，无法本地编译"; exit 1; }

  # 先清空上一次的源码目录，避免新旧文件混在一起 —— 也保证不会随部署次数累积。
  rm -rf "${BUILD_DIR}"
  mkdir -p "${BUILD_DIR}"
  log "解包源码到 ${BUILD_DIR}"
  tar xzf "${SRC_ARCHIVE}" -C "${BUILD_DIR}"

  [ -f "${BUILD_DIR}/go.mod" ] || { log "源码包里没有 go.mod，打包方式不对"; rm -rf "${BUILD_DIR}"; exit 1; }

  log "开始编译（GOPROXY=${GOPROXY}）"
  build_started="$(date +%s)"
  if ! ( cd "${BUILD_DIR}" && \
         GOPROXY="${GOPROXY}" GOFLAGS=-mod=mod GOTOOLCHAIN=auto CGO_ENABLED=0 \
         "${GO_BIN}" build -trimpath -ldflags "-s -w" -o "${NEW_BIN}" ./cmd/cloud-control ); then
    log "编译失败，未触碰容器，本次部署中止"
    rm -rf "${BUILD_DIR}"
    exit 1
  fi
  log "编译完成，耗时 $(( $(date +%s) - build_started )) 秒"

  # 只保留产物：源码目录用完即删（服务器空间紧张）。
  rm -rf "${BUILD_DIR}"
fi

[ -f "${NEW_BIN}" ] || { log "找不到新二进制：${NEW_BIN}"; exit 1; }

# 关键：docker cp 会保留宿主源文件的权限，而容器内以非 root（uid 10001）运行，
# 权限若不是可执行位，容器会因 "exec: permission denied" 直接起不来。
chmod 0755 "${NEW_BIN}"

# 自检是不是 Linux 可执行文件，避免把坏文件推进生产。
MAGIC="$(head -c 4 "${NEW_BIN}" | od -An -tx1 | tr -d ' \n')"
if [ "${MAGIC}" != "7f454c46" ]; then
  log "待部署文件不是 ELF 可执行文件（magic=${MAGIC}），中止部署"
  exit 1
fi

EXPECTED_SHA="$(sha256sum "${NEW_BIN}" | cut -d' ' -f1)"
log "新二进制 sha256=${EXPECTED_SHA}，大小 $(stat -c '%s' "${NEW_BIN}") 字节"

if [ "${DRY_RUN}" = "1" ]; then
  log "DRY_RUN=1，编译与校验均通过。实际部署将执行：备份 -> docker cp -> 校验 -> 重启 -> 健康检查 -> 固化镜像 -> 清理"
  # 只清理本次编译出来的产物；若调用方自带二进制（方式 B），原样保留。
  if [ -n "${SRC_ARCHIVE}" ]; then
    rm -f "${NEW_BIN}"
    rm -f "${SRC_ARCHIVE}"
  fi
  exit 0
fi

# ── 部署 ────────────────────────────────────────────────────
log "备份当前二进制到 ${PREV_BIN}（覆盖上一次的备份，只保留最近一份）"
docker cp "${CONTAINER}:${TARGET}" "${PREV_BIN}"

log "替换容器内二进制"
docker cp "${NEW_BIN}" "${CONTAINER}:${TARGET}"

ACTUAL_SHA="$(docker exec "${CONTAINER}" sha256sum "${TARGET}" | cut -d' ' -f1)"
if [ "${ACTUAL_SHA}" != "${EXPECTED_SHA}" ]; then
  log "容器内二进制校验不一致（期望 ${EXPECTED_SHA}，实际 ${ACTUAL_SHA}），回滚"
  restore_previous || true
  exit 1
fi
log "容器内二进制校验一致"

log "重启容器"
docker restart "${CONTAINER}" >/dev/null

if ! health_ok; then
  log "健康检查未通过"
  docker logs --tail 40 "${CONTAINER}" 2>&1 || true
  restore_previous || true
  exit 1
fi

if [ "${COMMIT}" = "1" ]; then
  # 固化进镜像，避免日后有人重建容器时倒退回旧二进制。
  # 注意：docker commit 会把容器当前的 Env 一并烘焙进镜像，
  # 因此该镜像不可外推到任何镜像仓库。
  log "固化进镜像 ${IMAGE}"
  docker commit "${CONTAINER}" "${IMAGE}" >/dev/null
fi

# ── 清理：只保留最新一份 ────────────────────────────────────
rm -f "${NEW_BIN}"
# 注意：不能写成 `[ -n ... ] && rm ...` —— 在 set -e 下该复合命令在条件为假时
# 返回非零，会让脚本直接退出。必须用 if。
if [ -n "${SRC_ARCHIVE}" ]; then
  rm -f "${SRC_ARCHIVE}"
fi
if [ "${PRUNE}" = "1" ]; then
  # 每次 commit 都会让上一个 tag 镜像变成悬空层；不清就会随部署次数累积。
  # 正在运行的容器所依赖的镜像不会被删除，所以这里只回收真正无用的层。
  pruned="$(docker image prune -f 2>/dev/null | tail -1 || true)"
  log "镜像清理：${pruned:-无悬空层}"
fi

log "部署完成：${CONTAINER}"
