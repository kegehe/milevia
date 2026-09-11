#!/usr/bin/env bash
#
# 云端连接服务（cloud-control）部署脚本 —— 在服务器上执行，不在本地跑。
#
# 由 .github/workflows/deploy-cloud-control.yml 通过 ssh 调用，也可人工断网急救时手动跑：
#   bash /tmp/deploy-cloud-control.sh
#
# 流程：校验二进制 -> 备份旧版 -> docker cp 替换 -> 重启 -> 健康检查 -> 失败自动回滚。
#
# 可用环境变量覆盖：
#   CONTAINER   容器名，默认 milevia-cloud-control
#   NEW_BIN     新二进制路径，默认 /tmp/cloud-control-linux.new
#   PREV_BIN    旧二进制备份路径，默认 /tmp/cloud-control-prev
#   TARGET      容器内二进制路径，默认 /usr/local/bin/cloud-control
#   IMAGE       部署后固化的镜像名，默认 milevia-cloud-control:local
#   HEALTH_URL  健康检查地址，默认 http://127.0.0.1:8090/health
#   COMMIT      设为 0 可跳过 docker commit，默认 1
#   DRY_RUN     设为 1 只做前置校验并打印计划，不碰容器
#
set -euo pipefail

CONTAINER="${CONTAINER:-milevia-cloud-control}"
NEW_BIN="${NEW_BIN:-/tmp/cloud-control-linux.new}"
PREV_BIN="${PREV_BIN:-/tmp/cloud-control-prev}"
TARGET="${TARGET:-/usr/local/bin/cloud-control}"
IMAGE="${IMAGE:-milevia-cloud-control:local}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8090/health}"
HEALTH_RETRIES="${HEALTH_RETRIES:-20}"
COMMIT="${COMMIT:-1}"
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

[ -f "${NEW_BIN}" ] || { log "找不到新二进制：${NEW_BIN}"; exit 1; }
command -v docker >/dev/null || { log "服务器上没有 docker"; exit 1; }
docker inspect "${CONTAINER}" >/dev/null 2>&1 || { log "容器 ${CONTAINER} 不存在"; exit 1; }

# 关键：docker cp 会保留宿主源文件的权限，而容器内以非 root（uid 10001）运行，
# 权限若不是可执行位，容器会因 "exec: permission denied" 直接起不来。
chmod 0755 "${NEW_BIN}"

# 先自检上传的二进制是不是 Linux 可执行文件，避免把坏文件推进生产。
MAGIC="$(head -c 4 "${NEW_BIN}" | od -An -tx1 | tr -d ' \n')"
if [ "${MAGIC}" != "7f454c46" ]; then
  log "上传的文件不是 ELF 可执行文件（magic=${MAGIC}），中止部署"
  exit 1
fi

EXPECTED_SHA="$(sha256sum "${NEW_BIN}" | cut -d' ' -f1)"
log "新二进制 sha256=${EXPECTED_SHA}"

if [ "${DRY_RUN}" = "1" ]; then
  log "DRY_RUN=1，仅校验通过。实际部署将执行：备份 -> docker cp -> 校验 -> 重启 -> 健康检查 -> 固化镜像"
  exit 0
fi

log "备份当前二进制到 ${PREV_BIN}"
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
  docker image prune -f --filter "until=168h" >/dev/null 2>&1 || true
fi

rm -f "${NEW_BIN}"
log "部署完成：${CONTAINER}"
