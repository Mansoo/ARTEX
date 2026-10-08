package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**취약점 번호 규약**: finding_id 는 독립 취약점 레코드 ID이고, finding_node_id 는 탐색 노드 ID입니다. list_findings / list_task_findings / node_detail / get_task_node_detail 의 id 는 탐색 노드 ID로 유지되므로, 같은 반환값의 finding_id 에서 독립 번호를 읽어야 합니다. get_finding_traffic / bind_finding_traffic 는 독립 finding_id 를 사용합니다. 구버전 update_finding_report 의 finding_id 파라미터는 여전히 finding_node_id 를 전달합니다. report_finding 첫 줄의 숫자를 증거 도구에 사용하지 말고, 번호 오류가 발생했다고 해서 다른 숫자를 추측하지도 마세요."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n기본적으로 보고 Agent 가 보고서 작성 전에 트래픽을 대조하고 바인딩합니다. 보고자는 evidence 에 검증 명령, 핵심 출력, 이미 확보한 실제 트래픽 ID 및 그 용도를 남겨 보고 Agent 가 실행 기록과 대조해 확인하도록 합니다. 바인딩을 위해 추가로 패킷을 조회할 필요는 없습니다. 명시적 즉시 바인딩도 호환됩니다: traffic_refs 또는 evidence_hint_id 로 검증된 참조를 제출할 수 있으며, 후자는 이 작업에서 지정한 hint 의 구조화된 참조를 읽습니다. 둘 중 하나라도 유효하지 않으면 이번 보고는 전부 실패합니다. TCP/패킷 없음일 때는 이 선택 파라미터가 필요 없습니다. 반환되는 finding_id 와 finding_node_id 는 각각 독립 레코드와 탐색 노드를 나타냅니다."
		case "add_hint", "add_task_hint":
			note = "\n확인된 취약점을 인계할 때는 해당 프롬프트의 traffic_refs 에 검증된 트래픽의 ID, 용도, 설명, 순서를 남기고(단건은 최상위에, 배치는 대응하는 hints 요소에 넣음), text 에 그것이 증명하는 구체적 취약점을 설명합니다. 호출 측은 텍스트만 인계하고 기존 트래픽 참조를 버려서는 안 됩니다. 검증되지 않은 후보는 증거로 전달할 수 없습니다."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**트래픽 증거 인계(선택)**: 자동 바인딩은 기본적으로 보고 Agent 가 취약점 등록 후 보고서 작성 전에 수행합니다. 보고자는 evidence 에 검증 명령, 핵심 출력, 이미 확보한 실제 트래픽 ID 및 그 용도를 남기고 작업에 intent_id 를 포함해 보고 Agent 가 추적하기 쉽게 해야 합니다. 바인딩을 위해 추가로 패킷을 조회할 필요는 없습니다. Auto / Planner 가 대신 보고할 때는 실행자가 이미 확보한 참조를 버리지 마세요. add_hint / add_task_hint 로 traffic_refs 를 인계할 수 있으며, 명시적 즉시 바인딩은 여전히 report_finding 의 traffic_refs / evidence_hint_id 와 호환됩니다. TCP 이거나 패킷이 없을 때는 정상적으로 등록하고, ID를 추측하거나 패킷을 채우려고 반복 탐지해서는 안 됩니다."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\n플랫폼 대화에 작업 컨텍스트가 없을 때는 report_finding 을 직접 호출하지 말고, add_task_hint 로 이미 존재하는 대응 작업에 인계해 작업 Agent 가 등록하게 하며, list_task_findings 로 결과를 확인합니다."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n목표 완료를 판정하기 전에 이번에 확보한 증거의 보고/인계를 먼저 완료하세요. 증거 인계가 끝나지 않았는데 텍스트 취약점이 등록되었다는 이유만으로 작업을 종료하거나 Worker 를 취소하지 마세요. 패킷이 없을 때는 대기하거나 억지로 패킷을 수집할 필요가 없습니다."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**보고 전 트래픽 자동 연계(활성화됨)**: 이번에 트리거된 취약점에 대해 트래픽을 대조하고 바인딩한 뒤 보고서를 작성하는 것이 당신의 책임입니다. 먼저 report_finding 반환 JSON 또는 get_task_node_detail / list_task_findings 에서 명확한 finding_id 와 finding_node_id 를 가져옵니다. 취약점 상세, 대응 의도의 실행 기록, 기존 증거 목록을 읽고, 보고자가 인계한 실제 ID를 우선 사용합니다. 이번 검증이 HTTP이고 트래픽 도구를 사용할 수 있으면 traffic_search 로 후보를 선별하고 traffic_get 으로 요청/응답이 실제로 해당 취약점을 뒷받침하는지 하나씩 확인합니다. 도메인과 시간은 선별용일 뿐 귀속을 증명하지 않습니다. 확인된 증거를 재현 순서에 따라 bind_finding_traffic(finding_id, traffic_refs) 로 연계하고, baseline / proof / verification / supporting 중에서 선택해 용도를 설명합니다. 이번 취약점만 조작할 수 있으며, 취약점을 중복 생성하거나 대상을 다시 탐지하지 마세요. 바인딩 성공 후 get_finding_traffic 을 다시 호출해 최신 version 을 가져오고, 필요한 본문을 읽은 뒤 실제로 읽은 version 을 evidence_version 으로 update_finding_report 에 전달합니다(그 finding_id 파라미터는 여전히 finding_node_id 를 사용). 기존 바인딩은 중복 추가할 필요가 없습니다. TCP, 미수집, 도구 사용 불가, 정확히 일치하는 것이 없을 때는 자동 바인딩을 건너뛰고 텍스트/명령 증거에 근거해 정상적으로 보고서를 쓰며 이유를 설명하고, 트래픽을 맞추려고 추측해서는 안 됩니다. 바인딩에 실패하면 성공했다고 주장하지 말고, 기존 증거를 유지한 채 보고서에 바인딩하지 않은 이유를 설명하세요."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "선택: 이 프롬프트의 구체적 취약점에 대응하며 검증된 트래픽 참조이고, 순서를 유지합니다. 인계 후 report_finding 은 evidence_hint_id 로 이 참조들을 전달할 수 있습니다.", "items": obj(map[string]any{"traffic_id": str("실제 트래픽 ID"), "role": str("baseline / proof / verification / supporting"), "note": str("이 트래픽이 뒷받침하는 결론")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d는 이 작업의 프롬프트 노드여야 합니다(상속된 프롬프트는 바인딩에 직접 사용할 수 없음)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
