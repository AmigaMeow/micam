package main

// 查找任务用的「判定」调用。
//
// ===== 关键设计 =====
//
// 模型**只被允许输出三个字段**：match / note / confidence。
// 时间、设备、位置、事件 ID **一律由后端绑定**，模型无从生成。
// 这是「证据零编造」的架构保证 —— 不靠 prompt 约束约束，靠数据结构保证。

import (
	"context"
	"encoding/json"
	"strings"
)

// judgeVerdict 模型对单帧的判定
type judgeVerdict struct {
	Match      bool    `json:"match"`
	Note       string  `json:"note"`
	Confidence float64 `json:"confidence"`
}

// tokenUsage 供应商返回的实际用量
type tokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

const judgeSystemPrompt = `你是一个严谨客观的监控画面判定助手。

用户会给你一个查询问题，以及一张监控画面截图。
你只需要判断：这张画面里**是否出现了**用户描述的内容。

严格输出 JSON，只有三个字段：
{
  "match": true 或 false,
  "note": "一句话说明你看到了什么（不超过 30 字，严禁 Emoji）",
  "confidence": 0.0 到 1.0
}

反幻觉与判定规则：
- 严格以画面中肉眼可见的实体为准，绝不迎合用户问题
- 看不清、距离远、遮挡严重或夜视模糊无法确认物种/目标时，必须 match=false 且 confidence 给低值
- 飞虫掠过、蛛丝晃动、车灯反射属于环境噪点，严禁误判为人员或动物
- 不要推测时间、地点、人物身份，只客观判定画面中是否有目标
- 不要输出任何其他字段`

// Judge 对一张图做判定。
//
// 返回：判定结果、token 用量、错误。
// 注意：即使返回错误，usage 也可能非零（供应商已计费），
// 所以调用方必须用 usage 记账，不能只看 err。
func (c *aiProviderClient) Judge(ctx context.Context, question, target string, images [][]byte) (judgeVerdict, tokenUsage, error) {
	var v judgeVerdict
	var u tokenUsage

	var parts []oaiContentPart
	parts = append(parts, oaiContentPart{Type: "text", Text: "查询问题：" + question + "\n请判断下面这张监控画面里是否出现了符合上述描述的内容。"})
	for _, img := range images {
		parts = append(parts, oaiContentPart{
			Type:     "image_url",
			ImageURL: &oaiImg{URL: "data:image/jpeg;base64," + b64(img)},
		})
	}

	r, err := c.do(ctx, oaiReq{
		Model: c.model,
		Messages: []oaiMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: parts},
		},
		MaxTokens:      1200,
		ResponseFormat: &oaiRespFmt{Type: "json_object"},
	})
	if err != nil {
		return v, u, err
	}

	// 先取 usage：判定失败也要能记账
	u.PromptTokens = r.Usage.PromptTokens
	u.CompletionTokens = r.Usage.CompletionTokens

	msg := r.Choices[0].Message
	txt := msg.Content
	if strings.TrimSpace(txt) == "" {
		txt = msg.ReasoningContent
	}
	var parsed judgeVerdict
	if err := json.Unmarshal([]byte(stripFence(txt)), &parsed); err != nil {
		return v, u, err
	}
	parsed.Note = truncate(strings.TrimSpace(parsed.Note), 60)
	return parsed, u, nil
}
