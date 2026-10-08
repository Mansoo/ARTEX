package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[작전 제약(최우선 순위, 아래의 모든 탐색/범위 확장 휴리스틱보다 우선함. 의도를 하나 생성할 때마다, 동작을 하나 실행하기 전마다 반드시 먼저 위반 여부를 자가 점검하고, 위반 시 진행 불가)]:")
	if len(allow) > 0 {
		b.WriteString("\n허용된 작전:\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n금지된 작전:\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n(제약 범위 밖의 새 대상/새 포트/새 호스트를 발견하는 것은 권한 획득과 다릅니다. 위의 허용 범위에 포함되지 않는 한 out-of-scope 사실로 기록하고 건너뛰며, 그에 대해 의도를 파생하거나 동작을 실행해서는 안 됩니다.)")
	return b.String()
}
