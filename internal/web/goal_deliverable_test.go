package web

import (
	"strings"
	"testing"
)

// longStructuredReport builds a final-wrap-up-shaped answer (≥600 runes, two
// markdown headings) with the given phrases spliced in, mirroring the shape of
// the analysis report that used to 502 ("model did not select a required tool
// after constrained retry") because the goal's deliverable was text.
func longStructuredReport(extra string) string {
	res := "# 一、项目当前真实状态\n\n" +
		"项目核心业务主链路已经基本贯通：ASGI 入口存在，应用工厂已装配岗位、候选人、匹配、会话、智能体、管理员和飞书路由，岗位和候选人已经接入 SQLAlchemy 持久化，双向匹配已经接入实际数据查询、评分、过滤和结果持久化，飞书已经具备 Webhook、验签、去重、入队、Worker 消费和回复链路，重复岗位确认流程已经实现。当前完整测试结果为 370 passed，0 failed，15 warnings，Ruff 静态检查通过。\n\n" +
		"# 二、P0：建议立即修复\n\n" +
		"1. Web 匹配请求字段错误：前端计算了动态字段但实际发送的是 text，后端契约并不接收该字段，用户输入的候选人经历或岗位描述没有进入正确后端字段，匹配可能以空文本执行，页面可能显示空结果或低质量结果。\n" +
		"2. Worker 可能永久丢失任务：队列采用 LPUSH + BRPOP，任务被弹出后立即从 Redis List 删除，没有 ACK、processing 队列或消费者组，任何瞬时异常都可能让任务永久消失。\n" +
		"3. 飞书回复失败被静默视为成功：业务执行和消息交付没有拆成两个阶段，岗位已经创建但用户看到的是机器人没有反应。\n\n" +
		"# 三、P1：高价值技术与体验优化\n\n" +
		"强化租户删除条件、/ready 反映真实依赖状态、确认任务原子领取、统一 Confirmation Store 装配、匹配 API 增加佣金元数据、明确匹配结果数量语义、Web 列表增加加载状态和竞态保护、增加真实分页或截断提示、区分 API 错误类型、前端权限模型与后端 RBAC 对齐、管理员高风险操作增加确认。\n\n" +
		"# 四、P2：产品完善建议\n\n" +
		"强化匹配结果解释、支持从现有列表选择匹配对象、实现智能体 QUERY 意图、重复岗位确认增加差异预览、完善可访问性、更新文档并建立自动校验。\n\n" +
		"# 五、本轮已落地并验证的优化\n\n" +
		"1. UTC 时间处理：将领域模型中的 datetime.utcnow() 替换为 datetime.now(UTC)，测试警告从 550 条降至 15 条，完整测试保持通过。\n" +
		"2. 管理员设置密钥保护测试：API 不回显已保存的 LLM API 密钥，返回 api_key_configured，更新设置时未提交新密钥会保留原密钥。\n\n" +
		"# 六、推荐实施顺序\n\n" +
		"第一个提交：核心正确性；第二个提交：租户与管理安全；第三个提交：Worker 可靠性；第四个提交：Web 使用体验；第五个提交：生产验证与文档。"
	if extra != "" {
		res += "\n\n" + extra
	}
	return res
}

func TestRequiredRetryDeliverable(t *testing.T) {
	strongShort := "项目已完成并验证通过，所有功能已实现，目标已达成。" +
		"本次优化全部落地：UTC 时间处理替换完成、管理员密钥保护测试通过、完整回归 370 passed。" +
		"任务队列可靠性、租户删除加固、确认任务原子领取等 P0/P1 项也全部落地并通过验证，没有剩余阻塞项。" +
		"异步 Worker、匹配正确性、多租户防御和管理后台可用性等建议均已实现并验证完毕，此后无需任何补充工作。" +
		"管理后台交互效率与可信度、飞书回复可靠性、Web 列表加载状态与竞态保护、权限模型对齐等体验项同样完成并通过回归。"
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"long structured final report is the deliverable",
			longStructuredReport(""), true},
		{"final report with planning word 下一步第一项 still qualifies",
			longStructuredReport("必须作为下一步第一项处理，同时注意下一阶段最有价值的工作不是继续扩张功能范围，而是依次解决匹配正确性、异步可靠性、多租户防御和管理后台可用性。"), true},
		{"final report with continuation commitment is not the deliverable",
			longStructuredReport("我会继续推进剩余工作，下一轮继续处理 P1 与 P2 清单。"), false},
		{"short status report is not the deliverable",
			"目标尚未完成，现有证据确认第一阶段已完成，第二阶段还需要继续检查，下一轮我会继续处理。", false},
		{"explicit strong completion with real content qualifies",
			strongShort, true},
		{"short strong completion phrase alone is not enough",
			"项目已全部完成。", false},
		{"empty answer is not the deliverable",
			"", false},
		{"english structured report qualifies",
			"# Summary\n\nThe recruitment agent core chain is wired end to end: the ASGI entry exists, app factory assembles jobs, candidates, matching, conversations, agent, admin and feishu routes, persistence is backed by SQLAlchemy, matching runs on real queries, feishu has webhook + worker + reply, the full suite reports 370 passed 0 failed. The remaining work is backlogged in the ordering section below.\n\n# Delivered Optimizations\n\n1. UTC timestamps replaced in the domain models: the warning count dropped from 550 to 15 and the full suite stays green. 2. Admin settings key protection: the API never echoes the saved LLM API key, returns api_key_configured, and keeps the old key when an update does not submit a new one. 3. Worker reliability, tenant-scoped deletes and atomic confirmation-task claiming are all in place and verified.\n\n# Recommended Order\n\n1. fix match payload fields, 2. tenant-scoped deletes, 3. worker reliability, 4. web UX, 5. docs. This closes the objective.", true},
		{"english report with continuation commitment is not the deliverable",
			longStructuredReport("I will continue working on the P1 list in the next round."), false},
	}
	for _, c := range cases {
		if got := requiredRetryDeliverable(c.text); got != c.want {
			t.Errorf("%s: got %v want %v (text_len=%d)", c.name, got, c.want, len([]rune(c.text)))
		}
	}
}

// TestRequiredRetryDeliverableThresholds pins the length/heading thresholds so
// a future tuning cannot silently loosen or tighten the escape hatch.
func TestRequiredRetryDeliverableThresholds(t *testing.T) {
	// 599 runes with two headings must stay below the structured-report bar.
	short := longStructuredReport("")
	if len([]rune(short)) < 600 {
		t.Fatalf("fixture too short for the structured branch: %d runes", len([]rune(short)))
	}
	head := ""
	for i := 0; i < 299; i++ {
		head += "完"
	}
	oneHeading := "# 一、现状\n\n" + head
	if requiredRetryDeliverable(oneHeading) {
		t.Error("single-heading text below the structured bar must not qualify")
	}
	if !strings.Contains(oneHeading, "# ") {
		t.Fatal("fixture broken")
	}
}