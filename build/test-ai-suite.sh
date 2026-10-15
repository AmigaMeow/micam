#!/usr/bin/env bash
# AI 智能全功能自动化测试套件
set -euo pipefail

BASE_URL="http://127.0.0.1:8900"
PASS=0
FAIL=0

echo "=========================================================="
echo "      小米 NAS 摄像头 AI 智能端到端自动化测试套件"
echo "=========================================================="

test_endpoint() {
    local name="$1"
    local method="$2"
    local endpoint="$3"
    local body="$4"
    local match_pattern="$5"

    printf "▶ 测试: %-32s " "$name"
    local res=""
    if [ "$method" = "GET" ]; then
        res="$(curl -s -m 15 "$BASE_URL$endpoint" 2>&1 || true)"
    else
        res="$(curl -s -m 30 -X POST -H "Content-Type: application/json" -d "$body" "$BASE_URL$endpoint" 2>&1 || true)"
    fi

    if echo "$res" | grep -E -q "$match_pattern"; then
        echo "✅ PASS"
        PASS=$((PASS + 1))
    else
        echo "❌ FAIL"
        echo "   返回: $res"
        echo "   期望匹配: $match_pattern"
        FAIL=$((FAIL + 1))
    fi
}

# 1. 状态与配置
test_endpoint "AI 运行状态 (Status)" "GET" "/api/ai/status" "" "\"ready\":true"
test_endpoint "AI 核心配置 (Config)" "GET" "/api/ai/config" "" "\"hasKey\":true"

# 2. 外部模型连通性
test_endpoint "VLM 模型测试 (Provider)" "POST" "/api/ai/provider/test" "{}" "\"ok\":true"

# 3. 模块一：今日 AI 摘要 / 晨报
test_endpoint "今日 AI 概览与晨报" "GET" "/api/ai/overview" "" "\"today\":"

# 4. 模块二：自然语言秒级搜索
test_endpoint "搜索：普通时间词 (昨天)" "POST" "/api/ai/search" '{"q":"昨天有人吗"}' "\"provider\":\"local\""
test_endpoint "搜索：车牌号识别 (京A88888)" "POST" "/api/ai/search" '{"q":"京A88888"}' "京A88888"
test_endpoint "搜索：细粒度宠物活动 (小狗)" "POST" "/api/ai/search" '{"q":"小狗今天经过车位吗"}' "dog"

# 5. 模块三：车牌识别与检索
test_endpoint "LPR 引擎状态 (HyperLPR3)" "GET" "/api/ai/plate/status" "" "\"installed\":true"
test_endpoint "车牌检索 (京A88888)" "GET" "/api/ai/plate/search?plate=%E4%BA%ACA88888" "" "\"plate\":\"京A88888\""

# 6. 模块三：智能调查任务规划
test_endpoint "调查任务成本预估 (免费)" "POST" "/api/ai/find/estimate" '{"question":"昨天门口有车来过吗？"}' "\"ok\":true"

# 7. 模块四：成本与预算治理
test_endpoint "AI 用量与成本统计" "GET" "/api/ai/usage" "" "\"today\":"
test_endpoint "AI 自动分析工作器队列" "GET" "/api/ai/jobs" "" "\"running\":"

echo "----------------------------------------------------------"
echo "测试结果: 通过 $PASS 项, 失败 $FAIL 项"
echo "=========================================================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
