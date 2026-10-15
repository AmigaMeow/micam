package main

// Markdown 文档生成（供 /api/dev/docs）。
//
// 用缩进式代码块（不用反引号），避免转义麻烦。

import (
	"os"
	"path/filepath"
	"strings"
)

// markdownDocs 生成完整的接入文档。
// markdownDocs 返回 Markdown 文档。
//
// ===== 单一来源原则 =====
//
// 注意：这里**读磁盘上的 docs/API.md**，不是硬编码字符串。
// 早期版本把文档写在 Go 代码里，结果 docs/API.md 更新了、
// 线上接口还返回旧内容 —— 两份文档不同步，第三方客户端会踩坑。
//
// 查找顺序：
//  1. <appdest>/docs/API.md     （部署后的正式位置）
//  2. docs/API.md               （开发环境）
//  3. 内嵌兜底                  （万一都没找到）
func (s *Server) markdownDocs() string {
	cands := []string{}
	if appDestDir != "" {
		cands = append(cands, filepath.Join(appDestDir, "docs", "API.md"))
	}
	cands = append(cands, filepath.Join("docs", "API.md"))

	for _, p := range cands {
		if raw, err := os.ReadFile(p); err == nil && len(raw) > 200 {
			body := string(raw)
			// 顶部注入「本文档的实时地址」，方便第三方客户端知道去哪拿最新版
			head := "# MiCam API 接入文档\n\n" +
				"版本: " + appVersion + "\n\n" +
				"> **本文档有实时版本**，服务端更新后内容会自动变：\n" +
				">\n" +
				"> - Markdown：GET /api/dev/docs\n" +
				"> - OpenAPI 3.0：GET /api/dev/openapi\n" +
				"> - 文档版本/更新时间：GET /api/dev/docs/meta\n" +
				">\n" +
				"> 客户端建议在启动时拉一次，并对比 meta 里的 hash 判断有没有更新。\n\n" +
				"---\n\n"
			// 把原文件第一行标题替换掉（避免重复）
			body = strings.TrimPrefix(body, "# MiCam API 接入文档")
			body = strings.TrimLeft(body, "\n")
			// 去掉原来的「版本: x.y.z」行
			if strings.HasPrefix(body, "版本: ") {
				if i := strings.Index(body, "\n"); i > 0 {
					body = body[i+1:]
				}
			}
			return head + strings.TrimLeft(body, "\n")
		}
	}
	return s.markdownDocsEmbedded()
}

// markdownDocsEmbedded 内嵌兜底（找不到文件时用）
func (s *Server) markdownDocsEmbedded() string {
	return "# MiCam API 接入文档\n\n版本: " + appVersion + "\n\n" +
		"> ⚠️ 没找到 docs/API.md，以下为内嵌摘要。\n\n" +
		"完整文档请访问项目仓库：https://github.com/AmigaMeow/micam/blob/master/docs/API.md\n"
}
