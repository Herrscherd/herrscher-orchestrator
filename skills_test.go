package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Herrscherd/herrscher-contracts"
)

// skillMem is a hand-rolled contracts.Memory. These tests never touch a real
// vault, and a map keyed by node Key reproduces the only storage behaviour that
// matters here: Record is an upsert.
type skillMem struct {
	nodes    map[string]contracts.Node
	links    [][2]string
	recErr   error
	recalls  map[string]contracts.Subgraph
	searched []contracts.Query
}

func newSkillMem() *skillMem {
	return &skillMem{nodes: map[string]contracts.Node{}, recalls: map[string]contracts.Subgraph{}}
}

func (m *skillMem) Recall(_ context.Context, key string, _ int) (contracts.Subgraph, error) {
	if sg, ok := m.recalls[key]; ok {
		return sg, nil
	}
	if n, ok := m.nodes[key]; ok {
		return contracts.Subgraph{Root: n}, nil
	}
	return contracts.Subgraph{}, context.Canceled
}

func (m *skillMem) Record(_ context.Context, n contracts.Node) error {
	if m.recErr != nil {
		return m.recErr
	}
	m.nodes[n.Key] = n
	return nil
}

func (m *skillMem) Search(_ context.Context, q contracts.Query) ([]contracts.Node, error) {
	m.searched = append(m.searched, q)
	return nil, nil
}

func (m *skillMem) Links(_ context.Context, from, to, _ string) error {
	m.links = append(m.links, [2]string{from, to})
	return nil
}

func (m *skillMem) Unlink(context.Context, string, string) error { return nil }
func (m *skillMem) Close() error                                 { return nil }

func (m *skillMem) keys() []string {
	out := make([]string, 0, len(m.nodes))
	for k := range m.nodes {
		out = append(out, k)
	}
	return out
}

// skillCurator builds a scoped Curator over a stub with the feature on, which is
// what every test in this file needs.
func skillCurator(m contracts.Memory) *Curator {
	c := NewScoped(m, "s1", contracts.MemoryScope{Project: "projects/p", Agent: "agents/a"})
	c.SetLearnedSkills(true)
	return c
}

// activeSkill is a fixture: an active KindSkill node at key.
func activeSkill(key, title, body string) contracts.Node {
	return contracts.Node{Key: key, Kind: contracts.KindSkill, Title: title, Body: body,
		Meta: map[string]string{contracts.MetaState: contracts.StateActive}}
}

func TestReactWritesSkillNodePrivately(t *testing.T) {
	m := newSkillMem()
	c := skillCurator(m)

	out := c.React(context.Background(), "voilà\n<skill name=\"Retry HTTP\">\nattendre le Retry-After\n</skill>\nfini")

	n, ok := m.nodes["agents/a/skills/retry-http"]
	if !ok {
		t.Fatalf("no skill node written; got keys %v", m.keys())
	}
	if n.Kind != contracts.KindSkill {
		t.Errorf("Kind = %q, want %q", n.Kind, contracts.KindSkill)
	}
	if n.Body != "attendre le Retry-After" {
		t.Errorf("Body = %q", n.Body)
	}
	if n.Meta["capturedAt"] == "" || n.Meta[contracts.MetaLastSeen] == "" {
		t.Errorf("missing age stamps, so the node can never become promotable: %v", n.Meta)
	}
	if strings.Contains(out, "<skill") || strings.Contains(out, "Retry-After") {
		t.Errorf("marker leaked into the reply: %q", out)
	}
	if !strings.Contains(out, "voilà") || !strings.Contains(out, "fini") {
		t.Errorf("reply lost its prose: %q", out)
	}
	linked := false
	for _, l := range m.links {
		if l[0] == "agents/a" && l[1] == "agents/a/skills/retry-http" {
			linked = true
		}
		if l[0] == "projects/p" && l[1] == "agents/a/skills/retry-http" {
			t.Errorf("skill linked under the shared root")
		}
	}
	if !linked {
		t.Errorf("skill not linked under the agent root; links = %v", m.links)
	}
}

func TestReactSkillUpsertsInPlaceKeepingCapturedAt(t *testing.T) {
	m := newSkillMem()
	c := skillCurator(m)
	c.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	c.React(ctx, `<skill name="x">première version</skill>`)
	// An operator approved the first version. The revision below must not inherit
	// that decision: an approval binds to the body it was granted for.
	approved := m.nodes["agents/a/skills/x"]
	approved.Meta[MetaApproved] = "true"
	m.nodes["agents/a/skills/x"] = approved
	c.now = func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }
	c.React(ctx, `<skill name="x">seconde version</skill>`)

	if got := len(m.nodes); got != 1 {
		t.Fatalf("%d nodes, want 1 (a revision must upsert, not duplicate): %v", got, m.keys())
	}
	n := m.nodes["agents/a/skills/x"]
	if n.Body != "seconde version" {
		t.Errorf("Body = %q, want the revision", n.Body)
	}
	if n.Meta["capturedAt"] != "2026-01-01T00:00:00Z" {
		t.Errorf("capturedAt = %q; revising a skill must not reset its maturity clock", n.Meta["capturedAt"])
	}
	if n.Meta[contracts.MetaLastSeen] != "2026-06-01T00:00:00Z" {
		t.Errorf("lastSeen = %q, want the revision time", n.Meta[contracts.MetaLastSeen])
	}
	if n.Meta[MetaApproved] != "" {
		t.Errorf("the revision kept the old approval, so a skill approved once could be rewritten into anything: %v", n.Meta)
	}
}

// The private scope is where the whole trust boundary lives: a skill is free in
// its agent's root and needs a human to cross into the project's. A session with
// no agent root has no free side, and contracts.RecordPrivate would file the node
// under the project instead. Dropping it is the only answer that does not hand
// every agent of the project an unapproved procedure.
func TestReactSkillNeedsAPrivateScope(t *testing.T) {
	m := newSkillMem()
	c := NewScoped(m, "s1", contracts.MemoryScope{Project: "projects/p"})
	c.SetLearnedSkills(true)

	out := c.React(context.Background(), `avant <skill name="x">un corps</skill> après`)

	if len(m.nodes) != 0 {
		t.Errorf("wrote %v, want nothing: with no agent root this lands in the shared scope", m.keys())
	}
	if !strings.Contains(out, "avant") || !strings.Contains(out, "après") {
		t.Errorf("the reply lost its prose: %q", out)
	}
	if strings.Contains(out, "<skill") {
		t.Errorf("marker not stripped: %q", out)
	}
}

func TestReactSkillRejectsUnusable(t *testing.T) {
	cases := []struct{ name, reply string }{
		{"no name attribute", `<skill>un corps sans nom</skill>`},
		{"empty name", `<skill name="">un corps</skill>`},
		{"unnameable name", `<skill name="!!! ---">un corps</skill>`},
		{"empty body", `<skill name="x">   </skill>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSkillMem()
			c := skillCurator(m)
			out := c.React(context.Background(), tc.reply)
			if len(m.nodes) != 0 {
				t.Errorf("wrote %v, want nothing", m.keys())
			}
			if tc.name != "no name attribute" && strings.Contains(out, "<skill") {
				t.Errorf("marker not stripped: %q", out)
			}
		})
	}
}

func TestReactSkillOffByDefault(t *testing.T) {
	m := newSkillMem()
	c := NewScoped(m, "s1", contracts.MemoryScope{Project: "projects/p", Agent: "agents/a"})

	out := c.React(context.Background(), `<skill name="x">un corps</skill>`)

	if len(m.nodes) != 0 {
		t.Errorf("wrote %v with the feature off", m.keys())
	}
	if !strings.Contains(out, "<skill") {
		t.Errorf("an unrecognised marker must survive verbatim rather than be eaten: %q", out)
	}
}

func TestReactSkillSurvivesAMemoryFailure(t *testing.T) {
	m := newSkillMem()
	m.recErr = context.DeadlineExceeded
	c := skillCurator(m)

	out := c.React(context.Background(), "avant <skill name=\"x\">un corps</skill> après")

	if !strings.Contains(out, "avant") || !strings.Contains(out, "après") {
		t.Errorf("a memory failure broke the reply: %q", out)
	}
}

func TestPreambleAnnouncesTheSkillMarkerOnlyWhenOn(t *testing.T) {
	m := newSkillMem()
	if on := skillCurator(m); !strings.Contains(on.frame(""), "<skill name=") {
		t.Errorf("feature on but the preamble never tells the model the marker exists")
	}
	off := NewScoped(m, "s1", contracts.MemoryScope{Project: "projects/p"})
	if strings.Contains(off.frame(""), "<skill name=") {
		t.Errorf("feature off but the preamble advertises a marker that does nothing")
	}
}

func TestSkillNameCapsALongName(t *testing.T) {
	got := skillName(strings.Repeat("mot ", 40))
	if r := []rune(got); len(r) > maxSkillNameRunes {
		t.Errorf("skillName produced %d runes, want <= %d", len(r), maxSkillNameRunes)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("skillName = %q, want no trailing separator", got)
	}
}

func TestAsLearnedSkillStampsPrivateSkillCandidates(t *testing.T) {
	cases := []struct {
		name    string
		in      Candidate
		private bool
		want    contracts.NodeKind
	}{
		{"private under skills/ becomes a skill",
			Candidate{Private: true, Node: contracts.Node{Key: "skills/retry-http", Kind: contracts.KindDecision}},
			true, contracts.KindSkill},
		{"private but not under skills/ is left alone",
			Candidate{Private: true, Node: contracts.Node{Key: "notes/a-preference", Kind: contracts.KindDecision}},
			true, contracts.KindDecision},
		{"shared under skills/ is left alone",
			Candidate{Private: false, Node: contracts.Node{Key: "skills/shared-thing", Kind: contracts.KindDecision}},
			true, contracts.KindDecision},
		{"a leading slash is still under skills/",
			Candidate{Private: true, Node: contracts.Node{Key: "/skills/x"}},
			true, contracts.KindSkill},
		{"skills as a name prefix is not skills as a segment",
			Candidate{Private: true, Node: contracts.Node{Key: "skillsets/x", Kind: contracts.KindDecision}},
			true, contracts.KindDecision},
		// Without an agent root the "private" candidate is filed under the project,
		// so stamping it a skill would publish it to every agent unapproved.
		{"no private scope leaves the candidate as it was",
			Candidate{Private: true, Node: contracts.Node{Key: "skills/retry-http", Kind: contracts.KindDecision}},
			false, contracts.KindDecision},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := asLearnedSkill(tc.in, tc.private).Node.Kind; got != tc.want {
				t.Errorf("Kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLearnedSkillsMergesBothScopesPrivateWinning(t *testing.T) {
	m := newSkillMem()
	m.recalls["projects/p"] = contracts.Subgraph{
		Root: contracts.Node{Key: "projects/p", Kind: contracts.KindProject},
		Nodes: []contracts.Node{
			activeSkill("projects/p/skills/shared-only", "t", "partagée"),
			activeSkill("projects/p/skills/both", "t", "version partagée"),
			{Key: "projects/p/notes/a-fact", Kind: contracts.KindDecision, Body: "un fait"},
		},
	}
	m.recalls["agents/a"] = contracts.Subgraph{
		Root:  contracts.Node{Key: "agents/a", Kind: contracts.KindAgent},
		Nodes: []contracts.Node{activeSkill("agents/a/skills/both", "t", "version privée")},
	}
	c := skillCurator(m)

	got, err := c.LearnedSkills(context.Background())
	if err != nil {
		t.Fatalf("LearnedSkills: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d skills, want 2 (a fact is not a skill, a name collision is not two skills): %v", len(got), got)
	}
	byName := map[string]contracts.Node{}
	for _, n := range got {
		byName[skillTail(n.Key)] = n
	}
	if byName["both"].Body != "version privée" {
		t.Errorf("collision resolved to %q, want the private copy", byName["both"].Body)
	}
	if byName["shared-only"].Body != "partagée" {
		t.Errorf("a promoted skill must reach this agent too; got %q", byName["shared-only"].Body)
	}
}

func TestLearnedSkillsSkipsInactive(t *testing.T) {
	stale := activeSkill("agents/a/skills/stale-one", "t", "vieille")
	stale.Meta[contracts.MetaState] = contracts.StateStale
	archived := activeSkill("agents/a/skills/archived-one", "t", "morte")
	archived.Meta[contracts.MetaState] = contracts.StateArchived
	implicit := contracts.Node{Key: "agents/a/skills/implicit", Kind: contracts.KindSkill, Body: "sans état"}

	m := newSkillMem()
	m.recalls["projects/p"] = contracts.Subgraph{Root: contracts.Node{Key: "projects/p"}}
	m.recalls["agents/a"] = contracts.Subgraph{
		Root:  contracts.Node{Key: "agents/a"},
		Nodes: []contracts.Node{stale, archived, implicit},
	}

	got, _ := skillCurator(m).LearnedSkills(context.Background())

	if len(got) != 1 || got[0].Key != "agents/a/skills/implicit" {
		t.Fatalf("got %v, want only the implicitly-active node (an absent state means active)", got)
	}
}

func TestLearnedSkillsSilentWhenOff(t *testing.T) {
	m := newSkillMem()
	m.recalls["agents/a"] = contracts.Subgraph{
		Root:  contracts.Node{Key: "agents/a"},
		Nodes: []contracts.Node{activeSkill("agents/a/skills/x", "t", "un corps")},
	}
	c := NewScoped(m, "s1", contracts.MemoryScope{Project: "projects/p", Agent: "agents/a"})

	got, err := c.LearnedSkills(context.Background())

	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want nothing and no error with the feature off", got, err)
	}
}

// The names the engine reports carry no key, and a key cannot be rebuilt from a
// name: the <skill> marker writes agents/a/skills/x, while an extractor candidate
// keeps the flat key it was distilled with and is only linked under that root.
// Both shapes must be stamped, or a skill used every day would age out as unused.
func TestSkillUsedAdvancesLastSeenForBothKeyShapes(t *testing.T) {
	m := newSkillMem()
	const old = "2020-01-01T00:00:00Z"
	for _, n := range []contracts.Node{
		{Key: "agents/a/skills/private-one", Kind: contracts.KindSkill, Body: "p",
			Meta: map[string]string{contracts.MetaLastSeen: old, "capturedAt": old}},
		{Key: "skills/distilled-one", Kind: contracts.KindSkill, Body: "d",
			Meta: map[string]string{contracts.MetaLastSeen: old}},
		{Key: "projects/p/skills/shared-one", Kind: contracts.KindSkill, Body: "s",
			Meta: map[string]string{contracts.MetaLastSeen: old}},
	} {
		m.nodes[n.Key] = n
	}
	m.recalls["agents/a"] = contracts.Subgraph{
		Root:  contracts.Node{Key: "agents/a"},
		Nodes: []contracts.Node{m.nodes["agents/a/skills/private-one"], m.nodes["skills/distilled-one"]},
	}
	m.recalls["projects/p"] = contracts.Subgraph{
		Root:  contracts.Node{Key: "projects/p"},
		Nodes: []contracts.Node{m.nodes["projects/p/skills/shared-one"]},
	}
	c := skillCurator(m)
	c.now = func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) }

	// The projection is what put these skills on disk in the first place, so it is
	// always the step before the engine can report one activated.
	if _, err := c.LearnedSkills(context.Background()); err != nil {
		t.Fatalf("LearnedSkills: %v", err)
	}
	c.SkillUsed(context.Background(), []string{"private-one", "distilled-one", "shared-one"})

	const want = "2026-08-25T12:00:00Z"
	for _, key := range []string{"agents/a/skills/private-one", "skills/distilled-one", "projects/p/skills/shared-one"} {
		if got := m.nodes[key].Meta[contracts.MetaLastSeen]; got != want {
			t.Errorf("%s lastSeen = %q, want %q", key, got, want)
		}
	}
	if got := m.nodes["agents/a/skills/private-one"].Meta["capturedAt"]; got != old {
		t.Errorf("capturedAt = %q, want it untouched: promotion measures maturity between the two stamps", got)
	}
	if got := m.nodes["agents/a/skills/private-one"].Body; got != "p" {
		t.Errorf("Body = %q, a usage stamp must not rewrite the skill", got)
	}
}

func TestSkillUsedIgnoresWhatItCannotStamp(t *testing.T) {
	m := newSkillMem()
	// Projected as a skill, but something else by the time it is stamped: the
	// projection is a session-old answer, so the kind is re-checked at write time.
	m.nodes["agents/a/skills/mutated"] = contracts.Node{Key: "agents/a/skills/mutated", Kind: contracts.KindDecision}
	m.recalls["projects/p"] = contracts.Subgraph{Root: contracts.Node{Key: "projects/p"}}
	m.recalls["agents/a"] = contracts.Subgraph{
		Root:  contracts.Node{Key: "agents/a"},
		Nodes: []contracts.Node{activeSkill("agents/a/skills/mutated", "t", "un corps")},
	}
	c := skillCurator(m)
	if _, err := c.LearnedSkills(context.Background()); err != nil {
		t.Fatalf("LearnedSkills: %v", err)
	}

	// A repo or machine-wide skill the engine reports by name and no node backs
	// must cost nothing, not a lookup per scope root.
	c.SkillUsed(context.Background(), []string{"never-heard-of-it", "!!!", "mutated", ""})

	if n := m.nodes["agents/a/skills/mutated"]; n.Meta[contracts.MetaLastSeen] != "" {
		t.Errorf("stamped a node that is not a skill: %v", n.Meta)
	}
	if len(m.nodes) != 1 {
		t.Errorf("SkillUsed created nodes: %v", m.keys())
	}
}

func TestSkillUsedIsInertWhenOff(t *testing.T) {
	m := newSkillMem()
	m.nodes["agents/a/skills/x"] = contracts.Node{Key: "agents/a/skills/x", Kind: contracts.KindSkill}
	c := NewScoped(m, "s1", contracts.MemoryScope{Project: "projects/p", Agent: "agents/a"})

	c.SkillUsed(context.Background(), []string{"x"})

	if m.nodes["agents/a/skills/x"].Meta[contracts.MetaLastSeen] != "" {
		t.Errorf("stamped with the feature off")
	}
}

func TestContextDoesNotReciteSkills(t *testing.T) {
	m := newSkillMem()
	m.recalls["projects/p"] = contracts.Subgraph{
		Root: contracts.Node{Key: "projects/p", Kind: contracts.KindProject, Title: "le projet"},
		Nodes: []contracts.Node{
			activeSkill("projects/p/skills/a-skill", "une skill", "LE CORPS DE LA SKILL"),
			{Key: "projects/p/notes/a-fact", Kind: contracts.KindDecision, Title: "un fait", Body: "LE CORPS DU FAIT"},
		},
	}
	m.recalls["agents/a"] = contracts.Subgraph{Root: contracts.Node{Key: "agents/a", Kind: contracts.KindAgent}}

	got := skillCurator(m).Context(context.Background())

	if strings.Contains(got, "LE CORPS DE LA SKILL") {
		t.Errorf("a skill is projected to disk and listed in the menu; reciting it here pays it twice:\n%s", got)
	}
	if !strings.Contains(got, "LE CORPS DU FAIT") {
		t.Errorf("the digest lost its facts:\n%s", got)
	}
}

func TestPromoteEligibleGatesSkillsOnApproval(t *testing.T) {
	mature := map[string]string{
		"capturedAt":           "2026-01-01T00:00:00Z",
		contracts.MetaLastSeen: "2026-06-01T00:00:00Z",
	}
	approved := map[string]string{MetaApproved: "true"}
	for k, v := range mature {
		approved[k] = v
	}

	l := &Learner{promoteMinAge: 24 * time.Hour}
	now := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		node contracts.Node
		want bool
	}{
		{"a mature fact needs no approval", contracts.Node{Kind: contracts.KindDecision, Meta: mature}, true},
		{"a mature skill without approval stays private", contracts.Node{Kind: contracts.KindSkill, Meta: mature}, false},
		{"a mature skill with approval crosses", contracts.Node{Kind: contracts.KindSkill, Meta: approved}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.promoteEligible(tc.node, now); got != tc.want {
				t.Errorf("promoteEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApproveSkillMarksAndUnmarks(t *testing.T) {
	m := newSkillMem()
	m.nodes["agents/a/skills/x"] = contracts.Node{
		Key: "agents/a/skills/x", Kind: contracts.KindSkill, Body: "un corps",
	}
	ctx := context.Background()

	if err := ApproveSkill(ctx, m, "agents/a/skills/x", true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if m.nodes["agents/a/skills/x"].Meta[MetaApproved] != "true" {
		t.Errorf("not approved: %v", m.nodes["agents/a/skills/x"].Meta)
	}
	if m.nodes["agents/a/skills/x"].Body != "un corps" {
		t.Errorf("approval rewrote the skill")
	}

	if err := ApproveSkill(ctx, m, "agents/a/skills/x", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, still := m.nodes["agents/a/skills/x"].Meta[MetaApproved]; still {
		t.Errorf("revoke left the mark: %v", m.nodes["agents/a/skills/x"].Meta)
	}
}

func TestApproveSkillRefusesWhatIsNotASkill(t *testing.T) {
	m := newSkillMem()
	m.nodes["projects/p/notes/a-fact"] = contracts.Node{
		Key: "projects/p/notes/a-fact", Kind: contracts.KindDecision,
	}
	err := ApproveSkill(context.Background(), m, "projects/p/notes/a-fact", true)
	if err == nil {
		t.Fatal("approved a node that is not a skill")
	}
	if !strings.Contains(err.Error(), "projects/p/notes/a-fact") {
		t.Errorf("the error must name the key: %v", err)
	}
}

func TestLearnedSkillsSettingIsDeclaredAndOffByDefault(t *testing.T) {
	var found *contracts.Setting
	for _, p := range contracts.Default.Orchestrators() {
		for i := range p.Manifest.Config {
			if p.Manifest.Config[i].Env == "MEMORY_LEARNED_SKILLS" {
				found = &p.Manifest.Config[i]
			}
		}
	}
	if found == nil {
		t.Fatal("MEMORY_LEARNED_SKILLS is not declared in the manifest, so `herrscher init` can never offer it")
	}
	if found.Key != "learned-skills" {
		t.Errorf("Key = %q, want %q", found.Key, "learned-skills")
	}
	if found.Required {
		t.Errorf("an opt-in feature must not be Required")
	}
	if boolSetting("", false) {
		t.Errorf("an unset MEMORY_LEARNED_SKILLS must read as off")
	}
}
