package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const maxSkillNameRunes = 60

// ErrNotSkill is wrapped by ApproveSkill when the node named by key is not a skill.
var ErrNotSkill = errors.New("not a skill")

var skillMarker = regexp.MustCompile(`(?is)<\s*skill\s+name\s*=\s*"([^"]*)"\s*>(.*?)<\s*/\s*skill\s*>`)

const skillPreamble = ` Write down a procedure you had to work out, so a later session starts where this one ended, ` +
	`with <skill name="short-name">the steps</skill>; re-emitting the same name revises it.`

// MetaApproved is a human's answer, set on a KindSkill node, to the only question
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

func (c *Curator) recordSkill(ctx context.Context, rawName, body string) {
	name := skillName(rawName)
	body = strings.TrimSpace(body)
	if name == "" || body == "" {
		return
	}
	scope := c.scopeOf()
	if scope.Agent == "" {
		slog.Warn("memory: a skill was written by a session with no private memory root, so it was dropped rather than shared with the project",
			"skill", name, "project", scope.Project)
		return
	}
	stamp := c.now().UTC().Format(time.RFC3339)
	node := contracts.Node{
		Key:   scope.Agent + "/skills/" + name,
		Kind:  contracts.KindSkill,
		Title: name,
		Body:  body,
		Meta:  map[string]string{contracts.MetaLastSeen: stamp, "capturedAt": stamp},
	}
	if sg, err := c.mem.Recall(ctx, node.Key, 0); err == nil && sg.Root.Meta["capturedAt"] != "" {
		node.Meta["capturedAt"] = sg.Root.Meta["capturedAt"]
	}
	c.logFailure("memory: record private skill failed", contracts.RecordPrivate(ctx, c.mem, scope, node))
}

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

func asLearnedSkill(candidate Candidate, hasPrivateScope bool) Candidate {
	if !candidate.Private || !hasPrivateScope {
		return candidate
	}
	if !strings.HasPrefix(strings.TrimPrefix(candidate.Node.Key, "/"), "skills/") {
		return candidate
	}
	candidate.Node.Kind = contracts.KindSkill
	return candidate
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
	if scope.Project == "" && scope.Agent == "" {
		return nil, nil
	}
	nodes, err := c.recallSkills(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := dedupeSkills(partitionSkills(nodes, scope.Agent))
	c.projectedMu.Lock()
	c.projected = projectedSkills(out)
	c.projectedMu.Unlock()
	return out, nil
}

func (c *Curator) recallSkills(ctx context.Context, scope contracts.MemoryScope) ([]contracts.Node, error) {
	if scope.Project != "" {
		sg, err := contracts.RecallScoped(ctx, c.mem, scope, 1)
		return sg.Nodes, err
	}
	sg, err := c.mem.Recall(ctx, scope.Agent, 1)
	return sg.Nodes, err
}

func partitionSkills(nodes []contracts.Node, agent string) skillPartition {
	var part skillPartition
	for _, node := range nodes {
		if !isActiveSkill(node) {
			continue
		}
		if agent != "" && strings.HasPrefix(node.Key, agent+"/") {
			part.private = append(part.private, node)
			continue
		}
		part.shared = append(part.shared, node)
	}
	return part
}

func isActiveSkill(node contracts.Node) bool {
	if node.Kind != contracts.KindSkill {
		return false
	}
	state := node.Meta[contracts.MetaState]
	return state == "" || state == contracts.StateActive
}

func dedupeSkills(part skillPartition) []contracts.Node {
	sort.Slice(part.private, func(i, j int) bool { return part.private[i].Key < part.private[j].Key })
	sort.Slice(part.shared, func(i, j int) bool { return part.shared[i].Key < part.shared[j].Key })
	out := make([]contracts.Node, 0, len(part.private)+len(part.shared))
	seen := make(map[string]bool, len(part.private)+len(part.shared))
	for _, node := range append(part.private, part.shared...) {
		name := skillTail(node.Key)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, node)
	}
	return out
}

func projectedSkills(skills []contracts.Node) map[string]string {
	projected := make(map[string]string, len(skills))
	for _, node := range skills {
		projected[skillTail(node.Key)] = node.Key
	}
	return projected
}

type skillPartition struct {
	private []contracts.Node
	shared  []contracts.Node
}

// SkillUsed refreshes lastSeen on each named skill. The host calls it with the
// names the skills engine saw activated in a reply, which makes this the whole of
// "improvement through use" in the sense that can actually be measured: nothing
// here judges whether the skill helped, only that it was reached for.
//
// The names are resolved against what LearnedSkills last answered rather than
// rebuilt from the scope roots, and that is what makes it correct rather than
// merely cheap. A skill's Key depends on how it was written: the <skill> marker
// keys under the agent root, while an extractor candidate keeps the flat key it
// was distilled with and is only *linked* under that root. A derived key finds
// the first and silently misses the second, so a skill the agent used every day
// would age out as unused. Resolving through the projection also means a repo or
// machine-wide skill, which the engine reports by the same name and which no node
// backs, costs no lookup at all.
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
	c.projectedMu.RLock()
	projected := c.projected
	c.projectedMu.RUnlock()
	if len(projected) == 0 {
		return
	}
	stamp := c.now().UTC().Format(time.RFC3339)
	for _, raw := range names {
		key := projected[strings.TrimSpace(raw)]
		if key == "" {
			continue
		}
		recalled, err := c.mem.Recall(ctx, key, 0)
		if err != nil || recalled.Root.Kind != contracts.KindSkill {
			continue
		}
		root := recalled.Root
		if root.Meta == nil {
			root.Meta = make(map[string]string)
		}
		root.Meta[contracts.MetaLastSeen] = stamp
		c.logFailure("memory: stamp skill failed", c.mem.Record(ctx, root))
	}
}

// ApproveSkill sets or clears the approval mark on the skill at key. It is a free
// function over a Memory rather than a Curator method because the host CLI runs
// it outside any session, exactly like Restore.
//
// Revoking does not undo a promotion that already happened: the shared copy is a
// node of its own, unmade with memory unlink and memory restore. Revoking only
// stops the next one, which is what a revoke can honestly promise.
func ApproveSkill(ctx context.Context, memory contracts.Memory, key string, approve bool) error {
	recalled, err := memory.Recall(ctx, key, 0)
	if err != nil {
		return fmt.Errorf("approve %s: %w", key, err)
	}
	if recalled.Root.Kind != contracts.KindSkill {
		return fmt.Errorf("approve %s: %w (kind %q)", key, ErrNotSkill, recalled.Root.Kind)
	}
	root := recalled.Root
	if root.Meta == nil {
		root.Meta = make(map[string]string)
	}
	if approve {
		root.Meta[MetaApproved] = "true"
	} else {
		delete(root.Meta, MetaApproved)
	}
	return memory.Record(ctx, root)
}

func skillTail(key string) string { return key[strings.LastIndex(key, "/")+1:] }
