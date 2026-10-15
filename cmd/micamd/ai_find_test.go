package main

import (
	"strings"
	"testing"
)

func TestBuildFindAnswer_NoEvidence_WithCoverage(t *testing.T) {
	task := &findTask{
		Total:    5,
		Evidence: nil,
		Coverage: findCoverage{
			Complete: true,
		},
	}
	ans := buildFindAnswer(task)
	if !strings.Contains(ans, "未发现符合描述") {
		t.Errorf("expected answer to contain '未发现符合描述', got %q", ans)
	}
}

func TestBuildFindAnswer_HitEvidence(t *testing.T) {
	task := &findTask{
		Total: 3,
		Evidence: []findEvidence{
			{EventID: "1", Match: true, Note: "发现车辆"},
		},
		Coverage: findCoverage{
			Complete: false,
			Note:     "请求的 2 天里只覆盖 1 天",
		},
	}
	ans := buildFindAnswer(task)
	if !strings.Contains(ans, "找到 1 处符合描述的记录") {
		t.Errorf("expected hit count in answer, got %q", ans)
	}
	if strings.Count(ans, "结果可能不全") > 1 {
		t.Errorf("found duplicate '结果可能不全' in answer: %q", ans)
	}
}
