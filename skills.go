package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Herrscherd/herrscher-contracts"
)

// maxSkillNameRunes caps a skill's key segment, and with it the directory the
// host projects it into. The model chooses the name, so without a cap one long
// sentence would become one very long path.
const maxSkillNameRunes = 60

// skillMarker matches the in-band marker the model emits to write one of its own
// skills. It mirrors rememberMarker and recallMarker in tolerance (case, space,
// multi-line) and adds the one thing they do not need: a name, because a fact is
// identified by its content and a procedure by what you call it.
var skillMarker = regexp.MustCompile(`(?is)<\s*skill\s+name\s*=\s*"([^"]*)"\s*>(.*?)<\s*/\s*skill\s*>`)

// skillPreamble is the sentence appended to memoryPreamble when the feature is
// on. It is separate so a build with the feature off never advertises a marker
// that would be ignored, which would teach the model a habit that does nothing.
const skillPreamble = ` Write down a procedure you had to work out, so a later session starts where this one ended, ` +
	`with <skill name="short-name">the steps</skill>; re-emitting the same name revises it.`

// MetaApproved, set on a KindSkill node, is a human's answer to the only question
// a self-authored skill raises: may every agent of this project run it. A skill's
// body comes from the journal, which carries chat messages, repository files and
// web pages, so a promoted one would turn "what this agent believes" into "what
// every agent executes". Private is free; shared is approved.
//
// Orchestrator-internal, like MetaPromotedTo and MetaMergedInto: obsidian stores
// Meta generically, so no contracts change is needed.
const MetaApproved = "approved"

// SetLearnedSkills turns the self-authored-skill feature on or off. Off (the
// default) leaves every surface inert: no marker, no preamble sentence, no
// normalisation of extractor candidates, and LearnedSkills answers nothing.
func (c *Curator) SetLearnedSkills(on bool) { c.learnedSkills = on }

// recordSkill writes one skill under the private scope. Best-effort in the same
// sense as remember: a memory failure is dropped, never returned, because the
// turn it happened in has already produced a reply someone is waiting for.
func (c *Curator) recordSkill(ctx context.Context, rawName, body string) {
	name := skillName(rawName)
	body = strings.TrimSpace(body)
	// An empty body would blank the previous version through the upsert, so a
	// truncated or malformed marker must not be able to erase a working skill.
	if name == "" || body == "" {
		return
	}
	scope := c.scopeOf()
	root := scope.Agent
	if root == "" {
		root = scope.Project
	}
	if root == "" {
		return
	}
	stamp := c.now().UTC().Format(time.RFC3339)
	node := contracts.Node{
		Key:   root + "/skills/" + name,
		Kind:  contracts.KindSkill,
		Title: name,
		Body:  body,
		Meta:  map[string]string{contracts.MetaLastSeen: stamp, "capturedAt": stamp},
	}
	// capturedAt is preserved on revision rather than restamped: promoteEligible
	// measures maturity as lastSeen minus capturedAt, so moving it forward every
	// time the skill is rewritten would reset the clock, and a skill revised often
	// would be the one that never matures.
	if sg, err := c.mem.Recall(ctx, node.Key, 0); err == nil && sg.Root.Meta["capturedAt"] != "" {
		node.Meta["capturedAt"] = sg.Root.Meta["capturedAt"]
	}
	_ = contracts.RecordPrivate(ctx, c.mem, scope, node)
}

// skillName folds a model-supplied name into a key segment. It is the same
// folding contracts.NormalizeScope applies, minus its "scope" fallback: a name
// that reduces to nothing must yield nothing, because a herd of unnamed skills
// all landing on one key would overwrite each other in silence.
func skillName(raw string) string {
	nameable := false
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			nameable = true
			break
		}
	}
	if !nameable {
		return ""
	}
	name := contracts.NormalizeScope(raw)
	if r := []rune(name); len(r) > maxSkillNameRunes {
		name = strings.TrimRight(string(r[:maxSkillNameRunes]), "-")
	}
	return name
}

// asLearnedSkill stamps KindSkill on an extractor candidate that is one. The
// decision lives here rather than in the extractor for two reasons: the
// extraction module is the closed half of the moat and a policy about node kinds
// has no business in it, and a private candidate can legitimately be a private
// *fact* rather than a procedure. The key prefix is the discriminant, read as a
// path segment so "skillsets/x" is not mistaken for a skill.
func asLearnedSkill(c Candidate) Candidate {
	if !c.Private {
		return c
	}
	if !strings.HasPrefix(strings.TrimPrefix(c.Node.Key, "/"), "skills/") {
		return c
	}
	c.Node.Kind = contracts.KindSkill
	return c
}

// LearnedSkills answers the active skills this session should have on disk: the
// agent's private ones and the project's shared ones, in one list.
//
// It is an OPTIONAL capability, discovered host-side by type assertion like
// SetScope and Consolidate before it, so contracts.Orchestrator gains no method
// and an orchestrator without it degrades to "no learned skills" rather than
// failing.
//
// Two rules are applied here rather than host-side, because they are about
// scopes and the host does not reason about scopes:
//
//   - Only active nodes are returned. A stale or archived skill must leave the
//     disk, or the staleness machine would be a label with no effect.
//   - On a name collision the private copy wins: an agent that refined its own
//     version of a shared procedure meant to use its version.
func (c *Curator) LearnedSkills(ctx context.Context) ([]contracts.Node, error) {
	if c.mem == nil || !c.learnedSkills {
		return nil, nil
	}
	scope := c.scopeOf()
	var (
		sg  contracts.Subgraph
		err error
	)
	switch {
	case scope.Project != "":
		sg, err = contracts.RecallScoped(ctx, c.mem, scope, 1)
	case scope.Agent != "":
		// RecallScoped roots on the project, so a session that has an agent but no
		// settled project reads the private root directly rather than reading none.
		sg, err = c.mem.Recall(ctx, scope.Agent, 1)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var private, shared []contracts.Node
	for _, n := range sg.Nodes {
		if n.Kind != contracts.KindSkill {
			continue
		}
		if s := n.Meta[contracts.MetaState]; s != "" && s != contracts.StateActive {
			continue
		}
		if scope.Agent != "" && strings.HasPrefix(n.Key, scope.Agent+"/") {
			private = append(private, n)
			continue
		}
		shared = append(shared, n)
	}
	// Sorted so the projection is byte-stable across runs: an unstable order would
	// rewrite identical files and make a diff of the projection root meaningless.
	sort.Slice(private, func(i, j int) bool { return private[i].Key < private[j].Key })
	sort.Slice(shared, func(i, j int) bool { return shared[i].Key < shared[j].Key })

	seen := make(map[string]bool, len(private)+len(shared))
	out := make([]contracts.Node, 0, len(private)+len(shared))
	for _, n := range append(private, shared...) {
		name := skillTail(n.Key)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, n)
	}
	return out, nil
}

// SkillUsed refreshes lastSeen on each named skill, in whichever scope holds it.
// The host calls it with the names the skills engine saw activated in a reply,
// which makes this the whole of "improvement through use" in the sense that can
// actually be measured: nothing here judges whether the skill helped, only that
// it was reached for.
//
// Two things fall out of that, and they are why this is worth a seam:
//
//   - The staleness sweep archives the skill nobody activates and leaves the one
//     that serves. A useless skill dies on its own, reversibly, with no retention
//     policy written anywhere.
//   - promoteEligible already requires lastSeen to have advanced past capturedAt.
//     A skill written once and never used therefore never becomes promotable, and
//     the existing eligibility rule reads, unmodified, as "this skill has served".
//
// Best-effort and silent: it runs after the reply is already on its way out.
func (c *Curator) SkillUsed(ctx context.Context, names []string) {
	if c.mem == nil || !c.learnedSkills || len(names) == 0 {
		return
	}
	scope := c.scopeOf()
	stamp := c.now().UTC().Format(time.RFC3339)
	for _, raw := range names {
		name := skillName(raw)
		if name == "" {
			continue
		}
		for _, root := range []string{scope.Agent, scope.Project} {
			if root == "" {
				continue
			}
			sg, err := c.mem.Recall(ctx, root+"/skills/"+name, 0)
			// Guarded on the kind, not just the key: a non-skill node that happens to
			// sit under a skills/ path must not be rewritten by a usage stamp.
			if err != nil || sg.Root.Kind != contracts.KindSkill {
				continue
			}
			n := sg.Root
			if n.Meta == nil {
				n.Meta = map[string]string{}
			}
			n.Meta[contracts.MetaLastSeen] = stamp
			_ = c.mem.Record(ctx, n)
		}
	}
}

// ApproveSkill sets or clears the approval mark on the skill at key. It is a free
// function over a Memory rather than a Curator method because the host CLI runs
// it outside any session, exactly like Restore.
//
// Revoking does not undo a promotion that already happened: the shared copy is a
// node of its own, unmade with memory unlink and memory restore. Revoking only
// stops the next one, which is what a revoke can honestly promise.
func ApproveSkill(ctx context.Context, m contracts.Memory, key string, approve bool) error {
	sg, err := m.Recall(ctx, key, 0)
	if err != nil {
		return fmt.Errorf("approve %s: %w", key, err)
	}
	if sg.Root.Kind != contracts.KindSkill {
		return fmt.Errorf("approve %s: not a skill (kind %q)", key, sg.Root.Kind)
	}
	n := sg.Root
	if n.Meta == nil {
		n.Meta = map[string]string{}
	}
	if approve {
		n.Meta[MetaApproved] = "true"
	} else {
		delete(n.Meta, MetaApproved)
	}
	return m.Record(ctx, n)
}

// skillTail is the name segment of a skill key ("agents/a/skills/x" -> "x").
func skillTail(key string) string { return key[strings.LastIndex(key, "/")+1:] }
