#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat <<'EOF'
逐个测试诊断页面中的已配置代理（依赖 bash、curl、jq）。

用法：
  bash scripts/test-proxies.sh [管理地址] [模型] [结果目录]
  bash scripts/test-proxies.sh http://127.0.0.1:8081 deepseek-v4.1-flash-free

默认管理地址：http://127.0.0.1:8081
默认模型：deepseek-v4.1-flash-free
运行时提示输入管理用户名、密码；也可设置 ADMIN_USERNAME、ADMIN_PASSWORD。
可用 KEY_TIER=zen 或 go 和 KEY_ID=指纹指定一个上游 Key，否则使用页面的自动路由。
每个代理测试一次，每次请求结束后等待 6 秒，完整结果保存在 results.jsonl。
最后列出实际 http_status=200 且出口匹配的代理，名称数组保存在 usable-proxies.json。
只测试服务器配置中的代理；127.0.0.1 等地址由网关服务器访问。
EOF
}

die() { printf '%s\n' "$*" >&2; exit 1; }
if [[ ${1:-} == -h || ${1:-} == --help ]]; then usage; exit 0; fi
[[ $# -le 3 ]] || die '参数过多；使用 --help 查看用法。'
for command in curl jq; do command -v "$command" >/dev/null || die "缺少依赖：$command"; done

admin_url=${1:-http://127.0.0.1:8081}
admin_url=${admin_url%/}
model=${2:-deepseek-v4.1-flash-free}
output=${3:-proxy-test-results-$(date +%Y%m%d-%H%M%S)}
[[ $admin_url == http://* || $admin_url == https://* ]] || die '管理地址必须以 http:// 或 https:// 开头。'
[[ -z ${KEY_ID:-} && -z ${KEY_TIER:-} || -n ${KEY_ID:-} && ${KEY_TIER:-} =~ ^(zen|go)$ ]] || die 'KEY_ID 与 KEY_TIER=zen/go 必须同时设置。'

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -z ${ADMIN_USERNAME:-} ]]; then
  read -r -p '管理用户名: ' ADMIN_USERNAME || die '无法读取用户名，请设置 ADMIN_USERNAME。'
fi
if [[ -z ${ADMIN_PASSWORD:-} ]]; then
  read -r -s -p '管理密码: ' ADMIN_PASSWORD || die '无法读取密码，请设置 ADMIN_PASSWORD。'
  printf '\n' >&2
fi
printf '%s' "$ADMIN_PASSWORD" | jq -Rs --arg username "$ADMIN_USERNAME" '{username: $username, password: .}' > "$work/login-request.json"
unset ADMIN_PASSWORD

http_code=$(curl --silent --show-error --connect-timeout 10 --max-time 30 \
  --cookie-jar "$work/cookies" --header 'Content-Type: application/json' \
  --data-binary "@$work/login-request.json" --output "$work/login-response.json" \
  --write-out '%{http_code}' "$admin_url/api/auth/login") || die '无法连接管理接口。'
[[ $http_code == 200 ]] || die "登录失败（HTTP $http_code）：$(jq -r '.error.message // "请检查地址和登录信息"' "$work/login-response.json" 2>/dev/null || true)"
csrf=$(jq -er '.csrf_token | select(type == "string" and length > 0)' "$work/login-response.json") || die '登录响应缺少 CSRF Token。'
printf 'header = "X-CSRF-Token: %s"\n' "$csrf" > "$work/auth.conf"
rm -f "$work/login-request.json" "$work/login-response.json"

get_json() {
  local path=$1 file=$2 code
  code=$(curl --silent --show-error --connect-timeout 10 --max-time 30 \
    --cookie "$work/cookies" --output "$file" --write-out '%{http_code}' "$admin_url$path") || die "无法读取 $path。"
  [[ $code == 200 ]] || die "读取 $path 失败（HTTP $code）。"
  jq -e 'type == "object"' "$file" >/dev/null || die "$path 没有返回 JSON 对象。"
}

get_json /api/config "$work/config.json"
get_json /api/debug/models "$work/models.json"
if [[ -z ${KEY_ID:-} ]] && jq -e '.warp.enabled == true' "$work/config.json" >/dev/null &&
  jq -e --arg model "$model" 'any(.models[]?; .model == $model and .anonymous == true)' "$work/models.json" >/dev/null; then
  die '该模型的匿名通道会优先走 WARP；请关闭 WARP，或用 KEY_TIER 和 KEY_ID 指定 Key 后再测试代理。'
fi
jq -e '.relays | type == "array" and all(.[]; type == "string")' "$work/models.json" >/dev/null || die '诊断接口缺少 relays 数组，请检查服务版本。'
count=$(jq '.relays | length' "$work/models.json")
[[ $count -gt 0 ]] || die '诊断页面没有可测试的代理。'
mkdir -m 700 -- "$output" || die '无法创建结果目录，请指定一个尚不存在的路径。'
results="$output/results.jsonl"
: > "$results"
printf '共 %s 个代理；模型 %s\n' "$count" "$model"
printf '序号\t接口HTTP\t实际HTTP\t耗时ms\t实际通道\t所选代理\t实际代理\t结果\n'
failed=0

for ((index=0; index<count; index++)); do
  [[ $index -eq 0 ]] || sleep 6
  relay=$(jq -r --argjson index "$index" '.relays[$index]' "$work/models.json")
  jq -n --argjson index "$index" --arg relay "$relay" --arg model "$model" \
    --arg tier "${KEY_TIER:-}" --arg id "${KEY_ID:-}" \
    '{protocol: "chat", key: (if $id == "" then {mode: "auto"} else {mode: "selected", tier: $tier, id: $id} end),
      relay: {index: $index, name: $relay},
      request: {model: $model, messages: [{role: "user", content: "Respond with the single word OK. Do not call tools."}], stream: false}}' > "$work/request.json"
  : > "$work/response.json"
  curl_error=""
  if http_code=$(curl --silent --show-error --connect-timeout 10 --max-time 180 \
    --cookie "$work/cookies" --config "$work/auth.conf" --header 'Content-Type: application/json' \
    --data-binary "@$work/request.json" --output "$work/response.json" --write-out '%{http_code}' \
    "$admin_url/api/debug/inference" 2> "$work/curl-error.txt"); then
    if ! jq -e 'type == "object"' "$work/response.json" >/dev/null 2>&1; then
      curl_error='管理接口没有返回 JSON 对象'
    fi
  else
    curl_error=$(cat "$work/curl-error.txt")
  fi
  if [[ -n $curl_error ]]; then
    jq -n --argjson index "$index" --arg relay "$relay" --arg code "$http_code" --arg error "$curl_error" \
      '{index: $index, relay: $relay, management_http_status: $code, result: {ok: false, error: {message: $error}}}' > "$work/row.json"
  else
    jq --argjson index "$index" --arg relay "$relay" --arg code "$http_code" \
      '{index: $index, relay: $relay, management_http_status: $code, result: .}' "$work/response.json" > "$work/row.json"
  fi
  jq '. + {relay_matches: (.result.relay_test == .relay)}' "$work/row.json" > "$work/checked-row.json"
  mv "$work/checked-row.json" "$work/row.json"
  jq -c . "$work/row.json" >> "$results"
  jq -r '[.index, .management_http_status, (.result.http_status // "—"), (.result.duration_ms // "—"),
    (.result.route | if .anonymous then "anonymous" else (.tier // "—") end), .relay, (.result.relay_test // "—"),
    (if .result.ok and (.relay_matches | not) then "实际出口与所选代理不符"
     else ((try (.result.response | if type == "object" then (.error.message // .choices[0].message.content)
       elif type == "string" then . else null end) catch null) // .result.error.message //
      (if .result.ok then "成功" else "失败" end)) end)] |
    map(if type == "array" or type == "object" then tojson else . end) | @tsv' "$work/row.json"
  if ! jq -e '.management_http_status == "200" and .result.ok == true and .relay_matches' "$work/row.json" >/dev/null; then failed=$((failed+1)); fi
  if [[ $http_code == 401 || $http_code == 403 ]]; then
    die "管理认证或 CSRF 失效，已停止；已完成结果保存在 $results。"
  fi
done

usable_proxies="$output/usable-proxies.json"
jq -s '[.[] | select(.management_http_status == "200" and .result.http_status == 200 and .relay_matches == true) | .relay]' \
  "$results" > "$usable_proxies"
printf '完成：成功 %s，失败 %s。完整结果：%s\n' "$((count-failed))" "$failed" "$results"
printf '可用代理（实际 HTTP 200）：%s 个。结果集合：%s\n' "$(jq 'length' "$usable_proxies")" "$usable_proxies"
jq -r '.[]' "$usable_proxies"
[[ $failed -eq 0 ]]
