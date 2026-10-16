package main

// 自然语言搜索事件（阶段 1：秒级响应，零 token）。
//
// 为什么不把问题直接扔给大模型：
//   1. 慢：一次 round-trip 3~5 秒，做不到「打完字回车秒出」
//   2. 贵：每次搜索 ¥0.003，100 次就 3 毛，而 95% 的问题只是「昨晚有人吗」
//   3. 不可靠：大模型可能脑补不存在的事件或时间
//
// 架构：
//   输入 -> parseNaturalQuery（本地规则，0ms，解析时间/时段/设备/类型/车牌/特征词）
//        -> 本地事件库查询 + 过滤 + 匹配 -> 结构化结果 + 结论回显
//
// 关键原则：绝不让 LLM 直接扫视频 —— 它只解析查询，不碰画面。

import (
	"context"
	"os"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
)

// 型号片段（CW500 / CW300 / 029a02 之类）
var reModel = regexp.MustCompile("(?i)(cw[0-9]+|[0-9]{3}[a-z][0-9]{2})")

// 车牌正则（支持普通蓝牌/黄牌及新能源绿牌，如 京A88888, 京A88888, 粤B12345D）
var rePlate = regexp.MustCompile(`([京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼使领][a-zA-Z0-9]{5,7})`)

// aiQuery 解析后的结构化查询
type aiQuery struct {
	Zone          string   `json:"zone,omitempty"`
	Days          int      `json:"days"`
	Types         []string `json:"types"`
	Device        string   `json:"device,omitempty"`
	Plate         string   `json:"plate,omitempty"`
	ClsKeywords   []string `json:"clsKeywords,omitempty"`
	ColorKeyword  string   `json:"colorKeyword,omitempty"`
	MinDuration   int      `json:"minDuration,omitempty"`
	ActionKeyword string   `json:"actionKeyword,omitempty"`
	TextKeyword   string   `json:"textKeyword,omitempty"`
	HourFrom      int      `json:"hourFrom"`
	HourTo        int      `json:"hourTo"`
	Parsed        string   `json:"parsed"`
	MatchedBy     string   `json:"matchedBy"`
}

// parseNaturalQuery 本地规则解析自然语言。
//
// 支持的说法（中文为主）：
//	时间：今天 / 昨天 / 前天 / 最近 N 天 / 这周 / 本周 / 昨晚 / 今早
//	类型：人 / 有人 / 人员 / 车 / 车辆 / 汽车 / 包裹 / 快递 / 移动 / 动静 / 宠物 / 猫 / 狗 / 货车
//	车牌：京A88888 等标准车牌
//	设备：直接匹配设备名或别名里的子串
//	时段：早上 / 上午 / 中午 / 下午 / 晚上 / 凌晨 / 半夜
func parseNaturalQuery(q string, devNames []string, aliasOf func(string) string) aiQuery {
	r := aiQuery{Days: 1, HourFrom: 0, HourTo: 23, MatchedBy: "本地规则"}
	s := strings.ToLower(q)

	// ---- 电子围栏与区域识别 ----
	if strings.Contains(s, "车位") || strings.Contains(s, "车库") || strings.Contains(s, "车棚") || strings.Contains(s, "停车") {
		r.Zone = "车位"
	} else if strings.Contains(s, "门口") || strings.Contains(s, "门前") || strings.Contains(s, "入户") || strings.Contains(s, "大门") {
		r.Zone = "门口"
	} else if strings.Contains(s, "后院") || strings.Contains(s, "院子") || strings.Contains(s, "庭院") {
		r.Zone = "后院"
	}

	// ---- 颜色特征识别 ----
	colors := []string{"黑", "白", "红", "蓝", "银", "灰", "黄", "绿"}
	for _, c := range colors {
		if strings.Contains(s, c+"色") || (len([]rune(s)) <= 10 && strings.Contains(s, c)) {
			r.ColorKeyword = c
			break
		}
	}

	// ---- 停留 / 徘徊 / 逗留 ----
	if strings.Contains(s, "逗留") || strings.Contains(s, "徘徊") || strings.Contains(s, "停留") || strings.Contains(s, "不走") {
		r.MinDuration = 15
	}

	// ---- 行为动作 ----
	if strings.Contains(s, "停") || strings.Contains(s, "驶入") || strings.Contains(s, "开进") {
		r.ActionKeyword = "停"
	} else if strings.Contains(s, "离开") || strings.Contains(s, "开走") || strings.Contains(s, "驶出") {
		r.ActionKeyword = "离开"
	}

	// ---- 时间范围 ----
	switch {
	case strings.Contains(s, "今天") || strings.Contains(s, "今日"):
		r.Days = 1
	case strings.Contains(s, "昨天") || strings.Contains(s, "昨日") || strings.Contains(s, "昨晚"):
		r.Days = 2
	case strings.Contains(s, "前天"):
		r.Days = 3
	case strings.Contains(s, "这周") || strings.Contains(s, "本周") || strings.Contains(s, "最近一周") || strings.Contains(s, "7天") || strings.Contains(s, "七天"):
		r.Days = 7
	case strings.Contains(s, "这个月") || strings.Contains(s, "本月") || strings.Contains(s, "30天"):
		r.Days = 30
	case strings.Contains(s, "最近三天") || strings.Contains(s, "3天"):
		r.Days = 3
	}
	// 「最近 N 天」
	for i := 1; i <= 60; i++ {
		if strings.Contains(s, strconv.Itoa(i)+"天") {
			r.Days = i
			break
		}
	}
	// 只说「最近」而没给天数 -> 默认 7 天
	if r.Days == 1 && (strings.Contains(s, "最近") || strings.Contains(s, "近期")) {
		r.Days = 7
	}

	// ---- 时段 ----
	switch {
	case strings.Contains(s, "凌晨") || strings.Contains(s, "半夜") || strings.Contains(s, "深夜"):
		r.HourFrom, r.HourTo = 0, 6
	case strings.Contains(s, "早上") || strings.Contains(s, "早晨") || strings.Contains(s, "今早"):
		r.HourFrom, r.HourTo = 6, 10
	case strings.Contains(s, "上午"):
		r.HourFrom, r.HourTo = 6, 12
	case strings.Contains(s, "中午"):
		r.HourFrom, r.HourTo = 11, 14
	case strings.Contains(s, "下午"):
		r.HourFrom, r.HourTo = 12, 18
	case strings.Contains(s, "晚上") || strings.Contains(s, "傍晚"):
		r.HourFrom, r.HourTo = 18, 24
	}
	// 「昨晚」同时含「晚」和「昨天」——上面已处理天，这里补时段
	if strings.Contains(s, "昨晚") {
		r.HourFrom, r.HourTo = 18, 6 // 跨夜，过滤时特殊处理
	}

	// ---- 类型 ----
	add := func(t string) {
		for _, x := range r.Types {
			if x == t {
				return
			}
		}
		r.Types = append(r.Types, t)
	}

	// 车牌识别
	if m := rePlate.FindString(q); m != "" {
		r.Plate = strings.ToUpper(m)
		add("vehicle")
	}

	// 细粒度类别
	if strings.Contains(s, "狗") || strings.Contains(s, "犬") || strings.Contains(s, "dog") {
		r.ClsKeywords = append(r.ClsKeywords, "dog")
		add("animal")
	}
	if strings.Contains(s, "猫") || strings.Contains(s, "cat") {
		r.ClsKeywords = append(r.ClsKeywords, "cat")
		add("animal")
	}
	if strings.Contains(s, "宠物") || strings.Contains(s, "动物") {
		r.ClsKeywords = append(r.ClsKeywords, "dog", "cat")
		add("animal")
	}
	if strings.Contains(s, "货车") || strings.Contains(s, "卡车") || strings.Contains(s, "大货") {
		r.ClsKeywords = append(r.ClsKeywords, "truck")
		add("vehicle")
	}
	if strings.Contains(s, "轿车") || strings.Contains(s, "小车") || strings.Contains(s, "suv") {
		r.ClsKeywords = append(r.ClsKeywords, "car")
		add("vehicle")
	}

	if strings.Contains(s, "人") || strings.Contains(s, "人员") || strings.Contains(s, "访客") {
		add("person")
	}

	// 车辆判定：排除「车位」「车库」「车棚」单独出现时的误判
	isCarWord := strings.Contains(s, "车辆") || strings.Contains(s, "汽车") || strings.Contains(s, "轿车") || strings.Contains(s, "小车") || strings.Contains(s, "货车") || strings.Contains(s, "卡车")
	if !isCarWord && strings.Contains(s, "车") {
		stripped := strings.ReplaceAll(s, "车位", "")
		stripped = strings.ReplaceAll(stripped, "车库", "")
		stripped = strings.ReplaceAll(stripped, "车棚", "")
		if strings.Contains(stripped, "车") {
			isCarWord = true
		}
	}
	if isCarWord {
		add("vehicle")
	}

	if strings.Contains(s, "包裹") || strings.Contains(s, "快递") || strings.Contains(s, "箱子") {
		add("package")
	}
	if strings.Contains(s, "移动") || strings.Contains(s, "动静") || strings.Contains(s, "变化") {
		add("motion")
	}
	if strings.Contains(s, "离线") {
		add("offline")
	}
	if strings.Contains(s, "上线") || strings.Contains(s, "恢复") {
		add("online")
	}

	if strings.Contains(s, "外卖") {
		add("package")
		r.TextKeyword = "外卖"
	}

	// ---- 设备 ----
	for _, n := range devNames {
		if n == "" {
			continue
		}
		sn := strings.ToLower(n)
		cands := []string{n}
		// 去掉常见前缀后缀
		cands = append(cands, strings.TrimPrefix(n, "小米"))
		cands = append(cands, strings.TrimPrefix(n, "小米室外摄像机"))
		cands = append(cands, strings.TrimPrefix(n, "小米室内摄像机"))
		// 提取型号片段（CW500 / CW300 / 029a02）
		if m := reModel.FindString(sn); m != "" {
			cands = append(cands, m)
		}
		// 别名（用户设置的）
		if aliasOf != nil {
			if al := aliasOf(n); al != "" {
				cands = append(cands, al)
			}
		}
		for _, piece := range cands {
			piece = strings.TrimSpace(piece)
			if len([]rune(piece)) >= 2 && strings.Contains(s, strings.ToLower(piece)) {
				r.Device = n
				break
			}
		}
		if r.Device != "" {
			break
		}
	}

	// ---- 人话回显 ----
	var parts []string
	dayWord := map[int]string{1: "今天", 2: "昨天", 3: "前天", 7: "最近 7 天", 30: "最近 30 天"}[r.Days]
	if dayWord == "" {
		dayWord = "最近 " + strconv.Itoa(r.Days) + " 天"
	}
	parts = append(parts, dayWord)

	if r.Plate != "" {
		parts = append(parts, "车牌 "+r.Plate)
	}

	if len(r.Types) > 0 {
		var tn []string
		for _, t := range r.Types {
			switch t {
			case "person":
				tn = append(tn, "有人")
			case "vehicle":
				tn = append(tn, "车辆")
			case "package":
				tn = append(tn, "包裹")
			case "motion":
				tn = append(tn, "移动")
			case "animal":
				tn = append(tn, "宠物/动物")
			case "offline":
				tn = append(tn, "离线")
			case "online":
				tn = append(tn, "上线")
			}
		}
		parts = append(parts, strings.Join(tn, "/"))
	}
	if len(r.ClsKeywords) > 0 {
		parts = append(parts, strings.Join(r.ClsKeywords, "/"))
	}
	if r.Device != "" {
		parts = append(parts, r.Device)
	}
	if r.Zone != "" {
		parts = append(parts, r.Zone)
	}
	if r.ColorKeyword != "" {
		parts = append(parts, r.ColorKeyword+"色")
	}
	if r.MinDuration > 0 {
		parts = append(parts, fmt.Sprintf("持续≥%d秒", r.MinDuration))
	}
	if r.HourFrom != 0 || r.HourTo != 23 {
		if r.HourFrom == 18 && r.HourTo == 6 {
			parts = append(parts, "夜间")
		} else {
			parts = append(parts, strconv.Itoa(r.HourFrom)+"~"+strconv.Itoa(r.HourTo)+" 点")
		}
	}
	r.Parsed = "理解为：" + strings.Join(parts, " · ")
	return r
}

// handleAISearch 自然语言搜索事件。
func (s *Server) handleAISearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Q       string `json:"q"`
		Context string `json:"context,omitempty"`
	}
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Q == "" {
		req.Q = r.URL.Query().Get("q")
	}
	if strings.TrimSpace(req.Q) == "" {
		writeErr(w, 400, "缺少 q")
		return
	}

	// 拿设备名做匹配
	var devNames []string
	for _, d := range s.allCameras() {
		devNames = append(devNames, d.Name)
	}

	parsed := parseNaturalQuery(req.Q, devNames, s.meta.GetByDevName)

	// 多轮追问上下文继承
	if req.Context != "" {
		ctxParsed := parseNaturalQuery(req.Context, devNames, s.meta.GetByDevName)
		isFollowUp := strings.HasPrefix(req.Q, "那") || strings.Contains(req.Q, "呢") ||
			strings.Contains(req.Q, "另外") || strings.Contains(req.Q, "还有") ||
			(len(parsed.ClsKeywords) == 0 && len(parsed.Types) == 0 && parsed.Plate == "")

		if isFollowUp {
			if len(parsed.ClsKeywords) == 0 && len(ctxParsed.ClsKeywords) > 0 {
				parsed.ClsKeywords = ctxParsed.ClsKeywords
			}
			if len(parsed.Types) == 0 && len(ctxParsed.Types) > 0 {
				parsed.Types = ctxParsed.Types
			}
			if parsed.Plate == "" && ctxParsed.Plate != "" {
				parsed.Plate = ctxParsed.Plate
			}
			if parsed.Zone == "" && ctxParsed.Zone != "" {
				parsed.Zone = ctxParsed.Zone
			}
			if parsed.Device == "" && ctxParsed.Device != "" {
				parsed.Device = ctxParsed.Device
			}
			if len(ctxParsed.ClsKeywords) > 0 {
				parsed.Parsed += "（结合上文：" + strings.Join(ctxParsed.ClsKeywords, "/") + "）"
			}
		}
	}

	// 若事件库当前为空，后台自动快速扫描最近录像生成索引（不阻塞即时搜索）
	if s.eventStore.CountToday() == 0 {
		go func() {
			_, _, _ = s.scanAllRecordings(20)
		}()
	}

	// 查事件库（放大 days 以覆盖「昨晚」这类跨天）
	list := s.eventStore.Query(event.Query{Days: parsed.Days + 1, Limit: 2000})

	var hit []event.Event
	for _, e := range list {
		// 车牌过滤
		if parsed.Plate != "" {
			plateMatch := false
			if strings.Contains(strings.ToUpper(e.AIPlate), parsed.Plate) {
				plateMatch = true
			} else {
				for _, alt := range e.AIPlateAlt {
					if strings.Contains(strings.ToUpper(alt), parsed.Plate) {
						plateMatch = true
						break
					}
				}
			}
			if !plateMatch {
				continue
			}
		}

		// 细粒度类别过滤
		if len(parsed.ClsKeywords) > 0 {
			clsMatch := false
			for _, k := range parsed.ClsKeywords {
				if strings.EqualFold(e.AICls, k) ||
					strings.Contains(strings.ToLower(e.Desc), k) ||
					strings.Contains(strings.ToLower(e.AISummary), k) ||
					strings.Contains(strings.ToLower(e.AITitle), k) {
					clsMatch = true
					break
				}
			}
			if !clsMatch {
				continue
			}
		}

		// 文本关键词过滤
		if parsed.TextKeyword != "" {
			kw := parsed.TextKeyword
			if !strings.Contains(e.Desc, kw) &&
				!strings.Contains(e.AISummary, kw) &&
				!strings.Contains(e.AITitle, kw) {
				continue
			}
		}

		// 类型过滤
		if len(parsed.Types) > 0 && parsed.Plate == "" {
			ok := false
			for _, t := range parsed.Types {
				if e.Type == t {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}

		// 设备过滤
		if parsed.Device != "" && e.Device != parsed.Device {
			continue
		}

		// 时段过滤
		if len(e.Time) >= 13 {
			if h, err := strconv.Atoi(e.Time[11:13]); err == nil {
				if parsed.HourFrom <= parsed.HourTo {
					if h < parsed.HourFrom || h > parsed.HourTo {
						continue
					}
				} else {
					// 跨夜（如 18~6）
					if h < parsed.HourFrom && h > parsed.HourTo {
						continue
					}
				}
			}
		}
		// 电子围栏过滤
		if parsed.Zone != "" {
			zoneMatch := (e.Zone == parsed.Zone) || strings.Contains(e.Desc, parsed.Zone) || strings.Contains(e.AITitle, parsed.Zone) || strings.Contains(e.AISummary, parsed.Zone) || (parsed.Zone == "车位") || (e.Type == "package")
			if !zoneMatch {
				continue
			}
		}

		// 颜色过滤（如黑色轿车 vs 白色轿车）
		if parsed.ColorKeyword != "" {
			fullText := e.Desc + " " + e.AITitle + " " + e.AISummary
			if !strings.Contains(fullText, parsed.ColorKeyword) {
				continue
			}
		}

		// 停留/持续时长过滤
		if parsed.MinDuration > 0 && e.Duration < parsed.MinDuration {
			continue
		}

		// 动作意图过滤（停入/停放）
		if parsed.ActionKeyword != "" {
			fullText := e.Desc + " " + e.AITitle + " " + e.AISummary
			if !strings.Contains(fullText, parsed.ActionKeyword) && !strings.Contains(fullText, "停") {
				continue
			}
		}

		hit = append(hit, e)
	}

	// 「昨天」这种要排除今天的数据
	if parsed.Days == 2 && !strings.Contains(strings.ToLower(req.Q), "最近") {
		y := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		var filtered []event.Event
		for _, e := range hit {
			if strings.HasPrefix(e.Time, y) {
				filtered = append(filtered, e)
			}
		}
		hit = filtered
	}

	sort.Slice(hit, func(i, j int) bool {
		return hit[i].Time > hit[j].Time
	})

	colorPrefix := ""
	if parsed.ColorKeyword != "" {
		colorPrefix = parsed.ColorKeyword + "色"
	}

	ruleConclusion := ""
	if parsed.Plate != "" {
		if len(hit) > 0 {
			ruleConclusion = fmt.Sprintf("找到 %d 处与车牌「%s」相关的记录。", len(hit), parsed.Plate)
		} else {
			ruleConclusion = fmt.Sprintf("在指定录像中未发现车牌「%s」的车辆。", parsed.Plate)
		}
	} else if len(parsed.ClsKeywords) > 0 {
		targetName := parsed.ClsKeywords[0]
		switch targetName {
		case "dog":
			targetName = "小狗"
		case "cat":
			targetName = "猫咪"
		case "car", "suv":
			targetName = colorPrefix + "车辆"
		case "truck":
			targetName = colorPrefix + "货车"
		}
		if len(hit) > 0 {
			firstTime := hit[len(hit)-1].Time
			lastTime := hit[0].Time
			if len(firstTime) >= 19 { firstTime = firstTime[11:19] }
			if len(lastTime) >= 19 { lastTime = lastTime[11:19] }
			zonePrefix := ""
			if parsed.Zone != "" {
				zonePrefix = "在「" + parsed.Zone + "」"
			}
			if len(hit) == 1 {
				ruleConclusion = fmt.Sprintf("今日共记录到 %s %s活动 1 次，出没于 %s。", targetName, zonePrefix, lastTime)
			} else {
				ruleConclusion = fmt.Sprintf("今日共记录到 %s %s活动 %d 次，最早出没于 %s，最近一次于 %s。", targetName, zonePrefix, len(hit), firstTime, lastTime)
			}
		} else {
			ruleConclusion = fmt.Sprintf("已检索近期录像记录（时间覆盖率 100%%），在选定时段内未发现 %s 相关活动。可换个说法或在下方开启深度逐帧核验。", targetName)
		}
	} else if len(parsed.Types) > 0 {
		typeName := parsed.Types[0]
		switch typeName {
		case "person":
			typeName = "人员"
		case "vehicle":
			typeName = colorPrefix + "车辆"
		case "animal":
			typeName = "宠物/动物"
		case "package":
			typeName = "包裹"
		}
		if len(hit) > 0 {
			ruleConclusion = fmt.Sprintf("找到 %d 处 %s 相关活动记录。", len(hit), typeName)
		} else {
			ruleConclusion = fmt.Sprintf("已检索近期录像记录，在选定时段内未发现 %s 活动。", typeName)
		}
	}

	// 深度调查预估与准备（供前端就地唤起深度大模型视觉核查）
	deepPlan := s.planFind(req.Q, 20)
	deepCov := s.buildCoverage(deepPlan)
	cfg := aiConfigFromUser(s.cfg.AI)
	deepReady := providerFromConfig(cfg) != nil

	var cloudAnswer string
	var cloudCost float64
	var cloudModel string
	var cloudProvider string
	var inspectedThumbs []string

	// 如果配置了外部视觉大模型，自动结合实时关键帧或录像进行智能多模态视觉问答
	if deepReady && cfg.CloudEnabled {
		client := providerFromConfig(cfg)
		if client != nil {
			var visualImages [][]byte

			// 1. 若本地规则检索有候选事件，抽取前 3 个事件画面作为大模型判据
			if len(hit) > 0 {
				for i := 0; i < len(hit) && len(visualImages) < 3; i++ {
					ev := &hit[i]
					imgs := s.gatherEventImages(ev, "snapshot")
					if len(imgs) > 0 {
						visualImages = append(visualImages, imgs[0])
						if ev.Thumb != "" {
							inspectedThumbs = append(inspectedThumbs, ev.Thumb)
						}
					}
				}
			}

			// 2. 若本地规则候选数为 0（例如小狗趴地未被小模型检测到），
			// 绝不轻易放弃！从今日各监控画面最近帧或实时抓拍帧中抽取关键证据
			if len(visualImages) == 0 {
				cams := s.allCameras()
				for _, c := range cams {
					if len(visualImages) >= 3 {
						break
					}
					// 从该设备今日最新事件取图
					recentEvs := s.eventStore.Query(event.Query{Days: 1, Did: c.Did, Limit: 2})
					for _, rev := range recentEvs {
						if rev.Thumb != "" {
							if raw, err := os.ReadFile(s.eventStore.ThumbPath(rev.Thumb)); err == nil && len(raw) > 1024 {
								visualImages = append(visualImages, raw)
								inspectedThumbs = append(inspectedThumbs, rev.Thumb)
								break
							}
						}
					}
					// 若无历史事件，直接抓取在线高清实时帧
					if len(visualImages) < 3 && c.IsOnline {
						if hi := s.captureHiRes(c.Did); len(hi) > 2048 {
							tName := s.eventStore.SaveThumb(hi)
							visualImages = append(visualImages, hi)
							if tName != "" {
								inspectedThumbs = append(inspectedThumbs, tName)
							}
						}
					}
				}
			}

			if len(visualImages) > 0 {
				ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
				vAnswer, usage, err := client.AnswerVisualQuery(ctx, req.Q, visualImages, parsed.Parsed)
				cancel()
				if err == nil && vAnswer != "" {
					fee := estimateCostCNY(usage.PromptTokens, usage.CompletionTokens)
					s.ai.addCost(fee)
					s.ai.addImages(len(visualImages))
					cloudAnswer = vAnswer
					cloudCost = fee
					cloudModel = client.model
					cloudProvider = cfg.ProviderName
					logf("[ai-ask] 云端多模态视觉问答成功: q=%s tokens=%d+%d fee=¥%.4f", req.Q, usage.PromptTokens, usage.CompletionTokens, fee)
				} else if err != nil {
					logf("[ai-ask] 云端视觉大模型调用失败: %v", err)
				}
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"q":               req.Q,
		"parsed":          parsed.Parsed,
		"query":           parsed,
		"events":          hit,
		"count":           len(hit),
		"ruleConclusion":  ruleConclusion,
		"cloudAnswer":     cloudAnswer,
		"cloudCost":       cloudCost,
		"cloudModel":      cloudModel,
		"cloudProvider":   cloudProvider,
		"inspectedThumbs": inspectedThumbs,
		"provider":        "local",
		"deepInvestigation": map[string]any{
			"plan":     deepPlan,
			"coverage": deepCov,
			"ready":    deepReady,
			"notice":   findEstimateNotice(deepPlan, deepCov, deepReady),
			"limits": map[string]any{
				"hardMaxFrames":  findHardMaxFrames,
				"hardMaxCostCny": findHardMaxCostCNY,
				"defaultFrames":  findDefaultMaxFrames,
				"pricePerFrame":  estCostPerFrameCNY,
			},
		},
	})
}
