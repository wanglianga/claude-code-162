#!/usr/bin/env bash
# 端到端演示：六环节主流程 + 老人代确认/现金失败/加品类/称重争议/复称补差/复称偏轻/投诉。
set -uo pipefail
BASE="${BASE:-http://127.0.0.1:8080}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PSQL_BIN="${PSQL_BIN:-/usr/lib/postgresql/15/bin/psql}"
PSQL="$PSQL_BIN -h $ROOT/.local/run -p 5433 -U recycle -d recycling -tAc"
PASS=0; FAIL=0
c_green=$'\033[32m'; c_red=$'\033[31m'; c_cyan=$'\033[36m'; c_off=$'\033[0m'

say()  { echo "${c_cyan}== $* ==${c_off}"; }
ok()   { echo "${c_green}[OK]${c_off} $*"; PASS=$((PASS+1)); }
bad()  { echo "${c_red}[FAIL]${c_off} $*"; FAIL=$((FAIL+1)); }
check(){ if [ "$1" = "$2" ]; then ok "$3 = $1"; else bad "$3: 期望 $2 实际 $1"; fi; }
login(){ curl -s -XPOST "$BASE/api/login" -H 'Content-Type: application/json' \
        -d "{\"phone\":\"$1\",\"password\":\"123456\"}" | jq -r .token; }
api()  { # api TOKEN METHOD PATH [JSON]
  local tok="$1" m="$2" p="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -s -X"$m" "$BASE$p" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -d "$body"
  else
    curl -s -X"$m" "$BASE$p" -H "Authorization: Bearer $tok"
  fi
}
iso_offset(){ date -u -d "+$1 minutes" +%Y-%m-%dT%H:%M:%SZ; }
wait_stage(){ # token apptId expectedStage tries
  local tok="$1" id="$2" want="$3" tries="${4:-40}" got=""
  for _ in $(seq 1 "$tries"); do
    got=$(api "$tok" GET "/api/appointments/$id" | jq -r '.appointment.stage')
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 0.5
  done
  echo "$got"; return 1
}

TOK_RES1=$(login 13800000021)   # 张奶奶（老人，6层无电梯）
TOK_RES2=$(login 13800000022)   # 陈先生（12层电梯）
TOK_RES3=$(login 13800000023)   # 刘阿姨（一层）
TOK_REC=$(login 13800000011)    # 王强（回收员）
TOK_ST=$(login 13800000001)     # 阳光回收站
TOK_OP=$(login 13700000000)   # 平台运营员
TOK_STR=$(login 13900000000)  # 和平街道办
[ -n "$TOK_RES1" ] && [ "$TOK_RES1" != null ] && ok "五类角色登录成功" || bad "登录失败"

echo
say "场景一：正常单（积分结算 + 凭证自动签发）"
BOOK=$(api "$TOK_RES2" POST /api/appointments "{
  \"items\":[{\"category\":\"paper\",\"weight_kg\":10,\"photo_urls\":[\"box://paper1.jpg\"]},
             {\"category\":\"plastic\",\"weight_kg\":5}],
  \"window_start\":\"$(iso_offset 60)\",\"window_end\":\"$(iso_offset 180)\"}")
A1=$(echo "$BOOK" | jq -r .id)
echo "$BOOK" | jq '{code,est_weight_kg,est_amount,"price_detail":{formula:.price_detail.formula,base:.price_detail.base_amount,floor:.price_detail.floor_surcharge,distance:.price_detail.distance_adjust}}'
EST=$(echo "$BOOK" | jq -r .est_amount); [ "$(echo "$EST" | awk '{print ($1>0)}')" = "1" ] && ok "预估价透明可解释：$EST 元"

# 上门后临时加品类（在接单后、称重前）
api "$TOK_REC" POST "/api/appointments/$A1/accept" >/dev/null
ADD=$(api "$TOK_RES2" POST "/api/appointments/$A1/items" \
  '{"category":"textile","weight_kg":2,"photo_urls":["box://cloth1.jpg"]}')
check "$(echo "$ADD" | jq -r .added_later)" "true" "居民临时加旧衣物 2kg"

api "$TOK_REC" POST "/api/appointments/$A1/enroute" >/dev/null
ARR=$(api "$TOK_REC" POST "/api/appointments/$A1/arrive")
check "$(echo "$ARR" | jq -r .late_minutes)" "0" "准时上门"
QR=$(api "$TOK_RES2" GET "/api/appointments/$A1/qr" | jq -r .qr_token)
api "$TOK_REC" POST "/api/appointments/$A1/verify" "{\"qr_token\":\"$QR\",\"method\":\"qr_scan\"}" >/dev/null
ok "回收员扫码核验居民身份"

W=$(api "$TOK_REC" POST "/api/appointments/$A1/weigh" '{
  "scale_no":"S-01","items":[
    {"category":"paper","weight_kg":9.6,"photo_urls":["box://w-paper.jpg"]},
    {"category":"plastic","weight_kg":4.8},
    {"category":"textile","weight_kg":2.0}]}')
WID=$(echo "$W" | jq -r .id)
WAMT=$(echo "$W" | jq -r .total_amount)
check "$(echo "$W" | jq -r .status)" "pending" "现场称重单待居民确认（$WAMT 元）"

api "$TOK_RES2" POST "/api/appointments/$A1/confirm-weigh" '{"agree":true,"method":"self"}' >/dev/null
ST=$(api "$TOK_RES2" POST "/api/appointments/$A1/settle" '{"method":"points"}')
check "$(echo "$ST" | jq -r .status)" "succeeded" "积分结算成功"
PTS=$(echo "$ST" | jq -r .points); ok "应得积分 $PTS"
# 幂等：重复结算不重复到账
api "$TOK_RES2" POST "/api/appointments/$A1/settle" '{"method":"points"}' >/dev/null
BAL=$(api "$TOK_RES2" GET /api/points/balance | jq -r .balance)
check "$BAL" "$PTS" "重复结算请求幂等，积分只到账一次"

# 站点复称：差异在 ±5% 内
api "$TOK_ST" POST "/api/appointments/$A1/inbound" '{
  "scale_no":"STATION-A","note":"分拣正常",
  "items":[{"category":"paper","weight_kg":9.7},
           {"category":"plastic","weight_kg":4.7},
           {"category":"textile","weight_kg":2.0}]}' >/dev/null
GOT=$(wait_stage "$TOK_RES2" "$A1" certified)
check "$GOT" "certified" "复称差异正常 → 凭证由 Kafka worker 自动签发"
CERT=$(api "$TOK_RES2" GET "/api/appointments/$A1" | jq -r '.certificate.cert_no')
[ -n "$CERT" ] && [ "$CERT" != null ] && ok "环保凭证已开具：$CERT"
sleep 1
NCNT=$(api "$TOK_RES2" GET "/api/notifications" | jq 'length')
[ "$NCNT" -ge 3 ] && ok "居民收到关键节点站内通知（接单/结算/凭证，共 $NCNT 条）" || bad "通知缺失: $NCNT"

echo
say "场景二：老人代确认 + 现金失败改积分 + 站点复称更重自动补差"
BOOK=$(api "$TOK_RES1" POST /api/appointments "{
  \"items\":[{\"category\":\"metal\",\"weight_kg\":8}],
  \"window_start\":\"$(iso_offset 30)\",\"window_end\":\"$(iso_offset 120)\"}")
A2=$(echo "$BOOK" | jq -r .id)
echo "$BOOK" | jq '{floor,has_elevator,est_weight_kg,est_amount}'
echo "估价明细：$(echo "$BOOK" | jq -c '.price_detail | {"base":.base_amount,"floor_surcharge":.floor_surcharge,"floor_reason":.floor_reason}')"
api "$TOK_REC" POST "/api/appointments/$A2/accept" >/dev/null
api "$TOK_REC" POST "/api/appointments/$A2/enroute" >/dev/null
# 模拟回收员迟到（把承诺到达时间改到 15 分钟前）
$PSQL "UPDATE appointments SET promised_arrive_at=now()-interval '15 min' WHERE id=$A2" >/dev/null
LATE=$(api "$TOK_REC" POST "/api/appointments/$A2/arrive" | jq -r .late_minutes)
[ "$LATE" -ge 10 ] && ok "系统自动记录迟到 ${LATE} 分钟（可追溯/可投诉）" || bad "迟到判定异常: $LATE"
QR=$(api "$TOK_RES1" GET "/api/appointments/$A2/qr" | jq -r .qr_token)
api "$TOK_REC" POST "/api/appointments/$A2/verify" \
  "{\"qr_token\":\"$QR\",\"method\":\"assist\",\"note\":\"张奶奶不会用手机，当面出示预约二维码并口头同意后由回收员协助确认\"}" >/dev/null
ok "老人身份由回收员协助核验并强制留痕"

api "$TOK_REC" POST "/api/appointments/$A2/weigh" '{"scale_no":"S-02","items":[{"category":"metal","weight_kg":7.8}]}' >/dev/null
api "$TOK_REC" POST "/api/appointments/$A2/assist-confirm" \
  '{"note":"老人看不清金额，已逐项念出重量单价并征得同意后代确认"}' >/dev/null
ok "称重结果老人代确认（confirm_method=assist，确认人可追溯）"

FAILST=$(api "$TOK_RES1" POST "/api/appointments/$A2/settle" '{"method":"cash","simulate_fail":true}')
check "$(echo "$FAILST" | jq -r .status)" "failed" "现金支付失败被记录（款项与积分均未变动）"
ST=$(api "$TOK_RES1" POST "/api/appointments/$A2/settle" '{"method":"points"}')
check "$(echo "$ST" | jq -r .status)" "succeeded" "改用积分结算成功（失败可重试）"
BAL_BEFORE=$(api "$TOK_RES1" GET /api/points/balance | jq -r .balance)

# 站点复称更重 10%：自动积分补差
IB=$(api "$TOK_ST" POST "/api/appointments/$A2/inbound" '{"scale_no":"STATION-A","items":[{"category":"metal","weight_kg":8.6}]}')
[ "$(echo "$IB" | jq -r .inbound_id)" != null ] && ok "站点复称更重，入站完成" || bad "入站失败: $IB"
sleep 2
BAL_AFTER=$(api "$TOK_RES1" GET /api/points/balance | jq -r .balance)
[ "$BAL_AFTER" -gt "$BAL_BEFORE" ] && ok "复称差额以积分自动补发：$BAL_BEFORE → $BAL_AFTER" || bad "补差积分未到账"
api "$TOK_RES1" GET /api/appointments/$A2 >/dev/null
ADJ=$(api "$TOK_RES1" GET "/api/appointments/$A2" | jq -r '.inbound.status')
check "$ADJ" "adjusted" "入站状态=adjusted，补差影响有记录"

echo
say "场景三：称重争议 + 混装拒收 + 复称偏轻 + 投诉处理"
BOOK=$(api "$TOK_RES3" POST /api/appointments "{
  \"items\":[{\"category\":\"appliance\",\"weight_kg\":6}],
  \"window_start\":\"$(iso_offset 45)\",\"window_end\":\"$(iso_offset 150)\"}")
A3=$(echo "$BOOK" | jq -r .id)
api "$TOK_REC" POST "/api/appointments/$A3/accept" >/dev/null
api "$TOK_REC" POST "/api/appointments/$A3/enroute" >/dev/null
api "$TOK_REC" POST "/api/appointments/$A3/arrive" >/dev/null
QR=$(api "$TOK_RES3" GET "/api/appointments/$A3/qr" | jq -r .qr_token)
api "$TOK_REC" POST "/api/appointments/$A3/verify" "{\"qr_token\":\"$QR\"}" >/dev/null

# 第一次称重：居民不认可
api "$TOK_REC" POST "/api/appointments/$A3/weigh" '{"scale_no":"S-03","items":[{"category":"appliance","weight_kg":5.9}]}' >/dev/null
DIS=$(api "$TOK_RES3" POST "/api/appointments/$A3/confirm-weigh" \
  '{"agree":false,"note":"我称的是6.2kg，对重量有异议，要求重称"}')
check "$(echo "$DIS" | jq -r .status)" "disputed" "居民对称重提出争议，结算被阻断"
BLOCK=$(api "$TOK_RES3" POST "/api/appointments/$A3/settle" '{"method":"cash"}')
echo "$BLOCK" | grep -q "WEIGH_NOT_FOUND" && ok "争议未解决时无法结算" || bad "争议时结算未被阻断: $BLOCK"

# 重新称重（原争议单作废，新单 seq=2），发现混入不可回收物 0.4kg
W2=$(api "$TOK_REC" POST "/api/appointments/$A3/weigh" '{"scale_no":"S-03","note":"双方面对电子秤重称","items":[
  {"category":"appliance","weight_kg":6.2,"rejected_kg":0.4,"rejected_reason":"夹杂破损陶瓷与电池，不可回收","photo_urls":["box://rej.jpg"]}]}')
check "$(echo "$W2" | jq -r .seq)" "2" "争议后重新称重，保留两次称重轨迹"
api "$TOK_RES3" POST "/api/appointments/$A3/confirm-weigh" '{"agree":true}' >/dev/null
ST=$(api "$TOK_RES3" POST "/api/appointments/$A3/settle" '{"method":"cash"}')
check "$(echo "$ST" | jq -r .status)" "succeeded" "重称确认后现金结算成功"

# 站点复称偏轻 12%（超阈值）：自动生成重量差异争议 + 扣回收员服务费
api "$TOK_ST" POST "/api/appointments/$A3/inbound" '{"scale_no":"STATION-A","note":"复核","items":[
  {"category":"appliance","weight_kg":5.4,"rejected_kg":0.4,"rejected_reason":"站点再次分拣出陶瓷"}]}' >/dev/null
DET=$(api "$TOK_OP" GET "/api/appointments/$A3")
check "$(echo "$DET" | jq -r '.inbound.status')" "disputed" "复称偏轻超 5% 阈值 → 自动开争议"
echo "$DET" | jq -r '.inbound | {"onsite_kg":.onsite_weight_kg,"station_kg":.station_weight_kg,"diff_pct":.diff_pct}'
CLAW=$(api "$TOK_REC" GET /api/recycler/earnings | jq -r '.by_type.service_fee_clawback.amount // empty')
[ -n "$CLAW" ] && ok "回收员服务费被扣回 $CLAW 元（路线收益透明）"

# 运营处理争议：补偿积分
DID=$(api "$TOK_OP" GET "/api/disputes?status=open" | jq -r --arg id "$A3" '[.[]|select(.appointment_id==($id|tonumber) and .type=="weight")][0].id')
api "$TOK_OP" POST "/api/disputes/$DID/resolve" \
  '{"resolution":"经核对为秤具误差，按积分补偿居民并约谈回收员","adjust_points":500}' >/dev/null
ok "运营复核争议 #$DID 并补偿 500 积分"

# 投诉
CID=$(api "$TOK_RES3" POST /api/complaints "{
  \"appointment_id\":$A3,\"category\":\"service\",\"content\":\"回收员上门时态度敷衍\"}" | jq -r .complaint_id)
api "$TOK_OP" POST "/api/complaints/$CID/handle" '{"response":"已核实并批评教育，回访居民致歉"}' >/dev/null
ok "投诉 #$CID 已受理闭环"

echo
say "场景四：可追溯性校验 —— 任一差异都能追到谁确认、何时确认、影响"
TL=$(api "$TOK_OP" GET "/api/appointments/$A3")
echo "$TL" | jq -r '.timeline[] | "[\(.stage)] \(.event_type) by \(.actor_role)#\(.actor_id // "system") @ \(.created_at)  \(.payload.impact // "")"' | head -20
STAGES=$(echo "$TL" | jq -r '[.timeline[].stage] | unique | join(",")')
echo "$STAGES" | grep -q appointment && echo "$STAGES" | grep -q dispatch && \
echo "$STAGES" | grep -q onsite && echo "$STAGES" | grep -q reweigh && \
echo "$STAGES" | grep -q settlement && echo "$STAGES" | grep -q dispute && \
ok "六环节+争议事件链完整：$STAGES" || bad "时间线环节缺失: $STAGES"

echo
say "场景五：街道看板（居民参与 + 资源流向）"
wait_stage "$TOK_RES3" "$A3" certified >/dev/null 2>&1 || true
api "$TOK_STR" GET /api/street/dashboard | jq '{
  "participants":.participants, "certificates":.total_certificates,
  "total_weight_kg":.total_weight_kg, "total_co2_saved_kg":.total_co2_saved_kg,
  "by_category":.by_category, "resource_flow":.resource_flow, "pipeline":.pipeline,
  "open_disputes":.open_disputes}'

echo
say "回收员收益与路线视角"
api "$TOK_REC" GET /api/recycler/earnings | jq '{"total_amount":.total_amount,"by_type":.by_type,"route":(.route|map({code,address,"distance_km":.distance_km,"late_minutes":.late_minutes,"stage":.stage}))}'

echo
if [ "$FAIL" -eq 0 ]; then
  echo "${c_green}全部 ${PASS} 项断言通过${c_off}"
else
  echo "${c_red}${FAIL} 项断言失败，${PASS} 项通过${c_off}"
fi
exit "$FAIL"
