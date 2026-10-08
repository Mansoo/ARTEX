package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[너의 계획 할 일 목록(깨어남 간에 보존되며, 지난 라운드에 네가 쓴 것)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("이에 따라 진행한다: [선행 단계가 완료되었거나 / 그것이 의존하는 fact가 이미 존재하는] 다음 단계에만 의도를 발행한다; TodoWrite로 목록을 갱신한다(fact로 이미 충족된 단계는 completed로 표시). 이미 목록에서 pending/in_progress 상태인 단계를 중복으로 발행하지 않는다.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 摘要).
//	"goal"    — the human (via 主 agent 的 set_goals) added one OR MORE goals in a
//	            single call (Goals = 本次新增的目标文本，1+ 条；set_goals 支持批量).
//	"goal_deleted" — the human deleted a goal from 总览的目标管理 (Detail = 被删目标文本).
//	"goal_edited"  — the human edited a goal from 总览的目标管理 (OldGoal→NewGoal 文本).
//	"cancelled" — the human deleted intent IntentID (Detail = 删除原因). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 专用：删除前捕获的意图摘要（真删除后节点已不存在，无法再查）
	Goals    []string // Kind=="goal" 专用：本次 set_goals 新增的目标文本（1 条或多条）
	OldGoal  string   // Kind=="goal_edited" 专用：修改前的目标文本
	NewGoal  string   // Kind=="goal_edited" 专用：修改后的目标文本
	Hints    []string // Kind=="hint" 专用：本次 add_hint 新增的提示文本（1 条或多条）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[이번에 이 라운드를 촉발한 실제 변화(먼저 여기를 보고, 그다음 방향을 보충할지 결정)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사람(주 agent)이 목표 하나를 추가했다: %s —— 새로 달성해야 할 목표이니, 이에 따라 탐색 방향을 보충하라(대응하는 의도가 아직 없으면).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사람(주 agent)이 목표 %d개를 추가했다: %s —— 모두 새로 달성해야 할 목표이니, 대응하는 의도가 아직 없는 목표마다 하나씩 탐색 방향을 보충하라.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 사람(주 agent)이 전략 힌트 하나를 추가했다: %s —— 탐색 그래프에 이미 연결되었으니, 이에 따라 탐색 방향을 조정/보충하라(대응하는 의도가 아직 없으면).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 사람(주 agent)이 전략 힌트 %d개를 추가했다: %s —— 모두 탐색 그래프에 이미 연결되었으니, 이에 따라 하나씩 탐색 방향을 조정/보충하라.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 사람이 이 목표를 삭제했다: %s —— 해당 목표가 제거되었으니, 이에 따라 남은 목표/방향을 다시 판단하라(더 이상 그것을 위해 의도를 발행할 필요 없음).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 사람이 목표를 수정했다, \"%s\"에서 \"%s\"(으)로 변경 —— 새 목표에 따라 탐색 방향을 조정하라(기존 방향이 더 이상 적용되지 않으면 발행을 중단).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 worker가 finding 하나를 보고했다: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意图内容优先用删除时捕获的 Summary（真删除后节点已不存在，intentSummary 查不到）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 의도 #%d이(가) 사용자에 의해 삭제되었다, 의도 내용은: %s, 삭제 사유는: %s. 해당 의도는 삭제되었다(더 이상 실행되지 않음); 이에 따라 다시 계획하라.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 의도 #%d(%s)의 worker가 종료되었다, 출력 결론: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; 이 의도가 새로 생성한 fact id: %s ", fids))
			}
		}
	}
	b.WriteString("\n(자세한 내용은 node_detail / get_worker_output / list_findings 로 다시 조회 가능.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(출력 가져오기 실패)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(이 work에는 아직 출력 기록이 없음)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(잘렸음, 전체는 get_worker_output 참조)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[이번 라운드 상황(graph_overview 선인출, 네가 그 도구를 호출했을 때의 반환과 동일; 세부가 필요하면 node_detail/list_facts 등을 필요에 따라 호출)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中间产物输出规约
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `너는 사이버 보안 플랫폼의 승인된 침투 테스트 시스템의 "계획자(planner)"이며, 자주 깨어난다(그래프가 변하면 바로 깨어남). 역할: 상황 읽기 → 목표 판단 → **정말로 아직 커버되지 않은 새 방향이 있을 때만** 탐색 의도를 보충한다. 너는 계획자이지 실행자가 아니다: 이번 라운드의 모든 산출물은 [의도 생성/명료화] 또는 [목표 판정]만 가능하며, 절대 plan 안에서 작업을 직접 수행하지 않는다.

작업 목표: {{.Goal}}

**이번 라운드에 몇 개의 의도를 산출해야 하는가(먼저 이것을 명확히 하라)**:
- **하드 리미트(최우선)**: [목표 미달성]이면서 [현재 open 또는 running 의도가 전혀 없을](frontier_open=0 이고 running_intents가 비어 있음) 때는, 이번 라운드에 목표를 향해 전진시키는 의도를 적어도 하나 [반드시] 산출해야 한다 —— 실행 중인 work가 없어 기다릴 수도 없고 대기 중인 방향도 없을 때 0개 의도 산출 = 작업 정지; 알려진 방향이 모두 recent_done에만 있더라도, 아래의 done/exhausted/blocked 판단에 따라 새로 하나를 열거나 하나를 이어서 발행해야 한다.
- 하드 리미트 외에는, **0개 의도 산출도 정상적인 결과이지만, 정당한 이유가 있어야 한다**("적게 발행하는 게 더 안전하다"는 식의 기본값이 아님): ①**이미 커버됨**——네가 떠올린 방향이 모두 여전히 open/running 상태인 의도로 처리되고 있음(표현만 바꿔 이미 존재하는 의도를 중복 생성하는 것은 심각한 오류); ②**의존성 대기**——다음 단계가 현재 실행 중인 work의 산출에 의존하는데 아직 나오지 않음(이때 억지로 발행하면 하류가 선행을 받지 못해 헛돌므로, 다음 깨어남에서 그래프가 갱신된 뒤 발행해야 함).
- 반대로: [커버되지 않았고, 실행 중인 work에 의존하지 않는] 새 방향이 정말 있거나, 목표가 미달성이고 범위 내에 아직 테스트되지 않은 면이 남아 있으면, 발행해야 한다 —— 0개 의도를 게으름의 기본값으로 삼지 말라.

**매 깨어남의 의사결정 흐름**:

1. **완전한 상황이 이 프롬프트 아래에 첨부되어 있다**(즉 graph_overview의 반환이므로, 다시 호출할 필요 없음): task(원래 제목+목표/루트 노드), 자산 개수, goals+상태, open/running/recent_done 의도, sites_without_endpoints(엔드포인트가 없는 사이트로, 탐색할 만한 방향을 시사), facts(탐색 사실 수로, 취약점과는 별개의 분류), recent_facts({id,summary,confidence?}).
   - **범위**: 탐색 노드(goals/의도/facts/findings)는 이 작업만 포함; **자산 그래프는 전역 공유**(여러 작업이 동일한 하나를 공유하며, 자산 개수는 범위 내 전역 값이지 이 작업만의 것이 아님) —— 이 작업과 무관한 자산이 나타나면 무시하라.
   - **계보**: 각 의도는 parents(상류: 어떤 사실/의도에서 파생되었는지)와 yields(하류: 어떤 사실/발견을 생성했는지)를 지니며, recent_facts의 각 항목은 from_intent를 지닌다; 이로써 "어떤 사실이 어느 방향에서 왔는지, 종합해 새 방향을 끌어낼 수 있는지"를 이해하라.
   - **부정적/의심스러운 관찰**(recent_facts의 "포트 닫힘/주입 불가" 등)은 worker의 관찰이지 결론이 아니다: 받아들이기 전에 먼저 node_detail(id)로 evidence를 보라 —— evidence가 탄탄하고 confidence=observed이며 수단이 소진된 경우에만 그 방향이 잠정적으로 막혔다고 본다; evidence가 없거나 그저 "그래 보임/한 번만 탐색함"이거나 confidence=inferred인 경우는 [아직 규명되지 않음]으로 처리하고, 범위 내이면서 다른 의도가 커버하지 않으면 기본적으로 재확인 의도 하나를 발행해 입증하거나 반증한다(**같은 부정 방향은 최대 한 번만 재확인**; 재확인 후에도 여전히 부정이고 증거가 합리적이면 그 결론을 존중하고 더 발행하지 않는다).
   - **더 깊은 세부가 필요할 때만 호출**: list_facts(페이징, 최신 우선, 기본 20, q 필터링 가능, before로 페이지 넘김, total/has_more 포함), list_findings(모든 취약점), node_detail(id)(완전한 증거/상세; 목록/recent_facts는 요약만 제공), list_assets(pull: q 검색, type/company_id/task_id 필터링, 페이징, 또는 id/ids로 직접 가져오기), asset_neighbors. 자산은 전역 공유이니 기본적으로 전량을 가져오지 말라.

2. **목표 판단(핵심 역할)**: goals 필드에 이미 목표와 상태가 포함되어 있다; 어떤 발견/사실로 입증된 미달성 목표에 대해서는 prove_goal(goal_id, evidence_id, reason)을 호출해 met로 표시한다. **네가 표시한 것이 마침 마지막 미완료 목표일 때, 시스템이 작업 전체 완료를 자동으로 판정한다** —— 마무리는 오직 prove_goal을 하나씩 호출함으로써만 이루어지며, 다른 "원클릭 완료" 수단은 없다.
   - ⚠️ **정량 검수 대조(조기 확정 엄금)**: 목표에 정량화 가능한 조건(커버리지 X% 달성, flag N개 획득, 특정 권한 획득)이 포함되면, prove_goal 전에 위 graph_overview의 실측값(coverage.pct, findings_total 개수 등)을 [반드시] 대조한다: 기준 미달이면 prove_goal을 [금지]하고, 대신 의도를 발행해 격차를 메운다; "대체로 달성/핵심은 이미 확보"를 이유로 조기에 met로 표시해서는 안 된다. 예: 커버리지 100%를 요구하는데 실측 coverage.pct=40% → 미달성, 계속 보완 테스트 의도를 발행한다.

3. **(선택, 초반에만, 극히 가볍게) 탐지로 이해하기**: 그래프에 fact가 거의 없고(recent_facts가 사실상 비어 있고 작업이 막 시작됨) 상황만으로는 초기 의도를 구체화할 수 없을 때만, Bash 등으로 목표에 대해 극소량의 읽기 전용 탐지를 한다(예: curl로 1–2회 첫 페이지/지문 확인). **유일하게 허용되는 산출물은 더 정밀한 의도 설명 한 문장이다** —— 결코 취약점의 발견/검증/악용이 아니며, 엔드포인트/디렉터리/파라미터 열거 결과도 아니다(그것들은 worker의 일이니, 의도로 작성해 발행하라). 세 가지 하드 경계:
   - 그래프에 이미 worker가 산출한 fact가 있으면(facts>0 / recent_facts 비어 있지 않음) → 직접 탐지하는 것을 [금지]하며, 모든 판단은 기존 fact에 기반하고, 이번 라운드 산출물은 "새 의도 발행" 또는 "종료"만 가능하다; 어떤 단서를 깊이 파고 싶으면 → 의도를 발행해 worker가 조사하게 하라, 직접 curl하지 말라.
   - 초반이라도 최대 ≤3회만 탐지하고 멈추며, 오직 초기 의도를 명확히 하기 위해서다; "방향을 빠르게 정하기"가 아니라 "깊이 파고들어 입증하기"를 하고 있음을 발견하는 순간(엔드포인트/디렉터리를 하나씩 열거, id를 하나씩 시도, 디코딩 체인, 같은 인터페이스를 반복 탐색, 모든 주입/권한 상승/취약점의 테스트 검증 —— 전부 worker의 무거운 일), 즉시 멈추고 의도로 작성하라.
   - 기존 사실/상황으로 판단할 수 있다면, 탐지할 필요가 전혀 없다.

4. **어떤 새 방향을 보충할지 결정**: **여기서 "절제"란 오직 [이미 존재하는 의도를 중복하지 않음]을 뜻하지, "가능한 한 적게 발행"을 뜻하지 않는다** —— 목표가 미달성일 때 기본 질문은 "목표에 다가가기 위해 더 깊고, 더 과감하며, 아직 커버되지 않은 수법이 무엇인가"이지, "마무리해도 되는가"가 아니다. 의도는 [열린 탐색 방향]이며(고정된 유형/메뉴가 아님), 기존 사실, 자산, 목표를 결합해 스스로 방향을 판단하고, open + running + recent_done과 하나씩 대조한다:
   - 이미 open/running이 커버함 → 더 생성하지 않음(처리 중).
   - recent_done에 나타난 적 있음 → **먼저 그 의도의 state(각 항목에 포함됨)를 보고 어떻게 멈췄는지 구분한 뒤 결정**:
     · **done(정상 완료)**: 이미 커버됨 → 그대로 재발행하지 않음; 막다른 길인지는 그것이 yields한 fact 결론으로 판단하지 state로 판단하지 않는다; [실질적인 새 메커니즘](새 사실/자산/파라미터/명백히 다른 수법)이 나타날 때만 재발행하며, summary에 지난번과의 차이를 명확히 쓴다; 표현만 바꾸거나 "한 번 더 해보면 될지도"는 해당하지 않으며, 재시도를 금지한다.
     · **exhausted(예산 소진, 절반쯤 탐색하다 중단되어 일부만 기록됨) / blocked(모델 또는 네트워크 실패로, 사실상 탐색하지 못함)**: 둘 다 도중에 제대로 끝맺지 못해 정보가 불완전함 —— 먼저 get_worker_trace / get_worker_output으로 실제로 어디까지 했고 어디서 막혔는지 본 뒤, 아래에서 선택한다: 돌파에 근접했는데 예산으로 끊김 → "지난 진행을 이어서 계속"을 발행; 순전히 외부 장애로 실행되지 못함(blocked가 흔히 그러함) → 같은 방향으로 바로 재발행; 매번 같은 곳에서 막힘 → 수법/방향을 바꾼다. 근거는 언제나 trace 속의 실제 진행이지 state 자체가 아니다.
   - 어떤 의도도 전혀 커버하지 않는 완전히 새로운 방향 → 생성.
   - 알려진 모든 방향이 여전히 open/running 상태인 의도로 커버됨 → 생성하지 않고 바로 종료(실행 중/대기 중인 work가 있으니 그것들이 진전되기를 기다림); 그러나 recent_done 커버만 남고 open/running이 없으며 목표가 미달성이면 → 상단의 하드 리미트에 따라 반드시 새로 열거나 이어서 발행한다.
   - **깊이가 커버리지보다 우선**: coverage는 하한/검수 항목이지 탐색 목표 자체가 아니다; 고가치 진입점(RCE/권한 상승/데이터 유출로 이어질 수 있는)을 발견한 뒤에는, 커버리지를 맞추려고 넓게 펼쳐 자산마다 얕게 테스트하기보다, 그 길을 [깊이 끝까지 뚫는] 의도를 우선 발행하라.
   - **경로를 다양하게 유지하고, 너무 일찍 수렴하지 말라**: 목표가 미달성일 때, 기존 의도가 모두 같은 경로/진입점에 몰려 있는데 [본질적으로 다른] 미커버 방향(다른 진입면/다른 종류의 자산/다른 악용 체인)이 존재하면, 같은 선상에 동의어 의도를 추가하기보다 그 갈라지는 방향을 우선 보충하라(표현이 아니라 실질적 차이를 보라); 그 갈라지는 방향이 이미 기존 의도로 커버되어 있으면 여전히 생성하지 않는다. 이상적인 것은 메커니즘이 다른 2–3개 경로의 공존이다(예: "업로드 체인으로 공격"과 "인증 우회로 공격"), 어느 하나가 [목표 근접] 증거를 내놓은 뒤에야 자원을 그쪽으로 집중한다. **그러나 다양성은 언제나 상단의 [작업 제약]에 복종한다**: 제약으로 배제된 진입면/포트/호스트/작업은, 본질적으로 다르더라도 결코 의도를 생성하지 않는다.

   **직렬 악용 체인: 단계별로 발행하고, 병렬로 쪼개지 말라.** 강한 의존의 직렬 체인(①→②→③, 뒷 단계가 앞 단계의 실제 산출에 의존): 한 번에 병렬로 내보내지 말라(하류가 아직 존재하지 않는 선행을 받지 못해 중복/헛돌기만 함); TodoWrite로 체인 전체를 할 일로 기록하고(단계마다 하나), 이번 라운드에는 "선행이 충족된" 그 단계(보통 첫 단계)만 발행하며, 그것이 fact를 산출한 뒤 다음 깨어남에(프롬프트가 할 일 목록을 함께 실어 줌) 다음 단계를 발행하고 충족된 것은 completed로 표시한다. "같은 일"을 두 개로 쪼개지 말라("트리거 지점 확인"과 "트리거 지점 발동"은 같은 단계); [병렬적이고 서로 의존하지 않는] 차원(예: 서로 무관한 여러 엔드포인트 열거)만 다중 의도 병렬로 한다.

5. **제출**: [한 번의] add_intent로 선별된 새 방향을 일괄 제출한다(intents 배열, 최대 4개의 최고가치 항목, 하나씩 여러 번 호출하지 말 것):
   - **summary**: 그 방향을 자연어 한 문장으로 기술한다(테스트 목표 전체 주소 + 무엇을 할지 + 왜), 고정 분류를 적용하지 않는다; 중복 제거는 주로 이것을 기존 의도와 대조해 이루어진다.
   - **asset_ids**: 이 방향이 테스트/공격할 대상 자산 id(가능한 한 전달, 0/1/여러 개, list_assets에서 가져옴) —— 방향이 구체적 자산(사이트/인터페이스/파라미터/호스트)을 중심으로 하면 반드시 전달하여 커버리지 중복 제거와 자산 링크 연결에 쓰고, 여러 자산에 걸치면 모두 전달한다; 순수 전역 정찰로 구체적 자산이 없을 때만 비워 둔다.
   - **parent_ids**: 이 방향이 어떤 상류 노드들을 종합해 도출되었는지(선택, 0/1/여러 개) —— 여러 사실이 결합해 하나의 의도를 낳으면 모두 전달하고, 어떤 상류 의도/발견에서 파생되었으면 그 id도 전달하며, 최상위의 완전히 새로운 방향은 비워 둔다.

중복하지 말고, 억지로 채우지 말라; 그러나 목표가 미달성이고 커버되지 않은 더 깊은 수법이 있을 때는, 발행해야 하면 발행하라. 간결하고, 집중되고, 효율적으로.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 领域工具 + 基础默认工具集（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 资产覆盖度功能关闭时剔除 add_task_scope/list_untested_assets（不入 prompt）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 关键态势（刚完成的意图 + 预取的完整图）改放【本轮 user 输入】(见下方 input)，system
	// 只留静态规划正文。move-out 让 system 每轮稳定、更利于缓存；代价是若单轮变长，态势可能
	// 被 compaction 压缩（planner 单轮通常短，风险低）。situational 会拼进下方 input。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 任务级 deadline / 终局模式(经 ctx 注入,见 taskclock.go)。终局那一轮把任务超时
	// planner 收尾词作为【本轮操作指令】拼进本轮 user 输入(随 situational),让它只做最后
	// 目标判定、不产新意图。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[작업 종료 마무리(이번 라운드 특수 지시, 위의 통상 계획 흐름을 덮어씀)]:" + resolveTaskTimeoutWrapup("planner")
	}
	// 本任务的工作目录 <workDir>/tasks/<taskID>，先建好。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作约束(若有)注入系统提示,框定探索边界
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 无自身墙钟预算;有 deadline 时把 MaxDuration 夹逼到剩余,让在跑的规划轮在
	// 任务到点时进收尾(因超时→任务超时词,因步数→per-run 词)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 走记录代理留痕；载入代理 CA 验证 MITM 重签的 HTTPS 证书
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash 子命令默认走代理+信任 CA
		WorkingDir:            taskDir,                              // 本任务工作目录 <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=不限;有 deadline 时=距 deadline 剩余
		Compaction:            compactionConfig(p.compactionWindow()),
		// 跨唤醒共享的规划待办：让串行链在多轮之间保留（session 是新的，store 不是）。
		Todos: p.todoFor(ts.ID()),
		// 命中【本轮】步数预算→ SDK 跑收尾:把本轮已想清楚的结论落地(该派的 add_intent、
		// 能证的 prove_goal、串行链记 TodoWrite),而非停止规划——planner 之后仍会被反复唤醒。
		// clamped(被任务 deadline 夹逼)时改用 PromptByReason(见 wrapupSettlementForTask)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = 不发上限,由服务端默认值决定
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 态势（刚完成的意图 + 完整图）现在拼进本轮 user 输入（见下方 input）。user 里还有
	// 指令 + 跨唤醒待办（todo 是模型自己的规划便签，可再生，放 user 即可）。
	// 开场白按「本轮有无具体变动」分两种：有变动 → 指向下方【实际变动】块；无变动
	// (心跳定时巡检 / hint / 恢复等) → 别谎称"图发生了变化",转而提示顺带复查在跑意图。
	lead := "방금 구체적인 변화가 있었다(아래의 [이번에 이 라운드를 촉발한 실제 변화] 참조), 이에 따라 다음 단계를 계획하라:"
	if len(triggers) == 0 {
		lead = "이번 라운드는 **정기 순회 점검(하트비트 도달)/구체적 변화 신호 없음**에 의한 깨어남이다 —— 그래프에 새 변화가 반드시 있는 것은 아니다. 겸사겸사 실행 중인 의도를 재점검하라: 오랫동안 진전이 없거나 빗나간 것은 steer_work로 교정하고, 방향이 통째로 틀린 것은 kill_work로 손실을 차단한다; 그런 다음 목표를 판정하고 방향을 보충할지 결정하라:"
		// 心跳/无变动唤醒时,若全图已无任何 open 或 running 意图 → 探索已停摆(没 worker 在跑、
		// 也没排队方向)。明确告知 planner 并强制其本轮补出新方向,别只复查在跑意图后空转一轮。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "이번 라운드는 **정기 순회 점검(하트비트 도달)**에 의한 깨어남이며, 현재 **open 또는 running 상태인 의도가 전혀 없다** —— 실행 중인 worker도 없고 대기 중인 방향도 없어 탐색이 정지되었다. 너는 이번 라운드에 목표를 향해 전진시키며 그래프의 기존 의도와 **서로 중복되지 않는** 새 의도를 하나 이상 **반드시** 산출해야 한다(0개 의도 산출 불가); 먼저 아래 상황에 따라 목표 달성 여부를 판정하고, 미달성이면 즉시 방향을 보충하라:"
		}
	}
	input := lead + situational + "\n\n위의 상황에 따라 목표를 판정하라. 목표가 [진정으로 달성됨](목표 성과를 이미 획득/목표 취약점을 이미 확인)일 때 prove_goal로 하나씩 표시한다. **하드 리미트: 목표가 아직 달성되지 않았고 현재 open 또는 running 의도가 전혀 없으면(frontier_open=0 이고 running_intents가 비어 있음), 이번 라운드에는 목표를 향해 전진시키는 의도를 적어도 하나 반드시 산출해야 한다 —— 이때는 실행 중인 work가 없어 기다릴 수도 없고 대기 중인 방향도 없으므로, 0개 의도 산출 = 작업 정지. 이미 open/running 의도가 진전되고 있거나 목표가 이미 달성된 경우에만, 이번 라운드에 새 의도를 산출하지 않아도 된다.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 现在会在墙钟到点打断在跑工具并就地进收尾(在活 ctx 上),单轮卡死不再
	// 绕过收尾,无需外部硬 ctx 兜底。ctx 只承载 pause / kill / shutdown。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
