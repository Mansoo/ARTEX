package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**취약점 트래픽 증거(선택)**: report_finding 으로 취약점을 보고할 때, 이미 확인했고 취약점 결론을 뒷받침한다고 판단한 HTTP 요청/응답이 있으면 traffic_refs 로 재현 순서에 따라 실제 ID를 바인딩할 수 있습니다. 도메인과 시간은 후보 선별용일 뿐 연관성을 단정하지 않습니다. TCP 등 비 HTTP 취약점이거나 수집되지 않았거나 정확히 일치하는 것이 없을 때는 생략하거나 []를 전달하고, evidence 에 명령 출력, 로그 등 검증 가능한 다른 증거를 남기며 바인딩하지 않은 이유를 설명하는 것이 좋습니다. ID를 추측하지 말고, 패킷을 채우려고 반복 탐지하지도 마세요."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
