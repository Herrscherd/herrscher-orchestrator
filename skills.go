package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const maxSkillNameRunes = 60

// ErrNotSkill is wrapped by ApproveSkill when the node is not a skill.
var ErrNotSkill = errors.New("not a skill")

var skillMarker = regexp.MustCompile(`(?is)<\s*skill\s+name\s*=\s*"([^"]*)"\s*>(.*?)<\s*/\s*skill\s*>`)

const skillPreamble = ` Write down a procedure you had to work out, so a later session starts where this one ended, ` +
	`with <skill name="short-name">the steps</skill>; re-emitting the same name revises it.`

// MetaApproved is set on a skill to approve sharing it with the project's
// agents. Set it through ApproveSkill.
const MetaApproved = "approved"

// SetLearnedSkills turns self-authored skills on or off. Off, the default,
// leaves every skill surface inert.
func (c *Curator) SetLearnedSkills(on bool) { c.learnedSkills = on }

func (c *Curator) recordSkill(ctx context.Context, rawName, body string) {
	name := skillName(rawName)
	body = strings.TrimSpace(body)
	if name == "" || body == "" {
		return
	}
	scope := c.scopeOf()
	if scope.Agent == "" {
		c.log.Warn("memory: a skill was written by a session with no private memory root, so it was dropped rather than shared with the project",
			"skill", name, "project", scope.Project)
		return
	}
	stamp := c.now().UTC().Format(time.RFC3339)
	node := contracts.Node{
		Key:   scope.Agent + "/skills/" + name,
		Kind:  contracts.KindSkill,
		Title: name,
		Body:  body,
		Meta:  map[string]string{contracts.MetaLastSeen: stamp, metaCapturedAt: stamp},
	}
	if sg, err := c.mem.Recall(ctx, node.Key, 0); err == nil && sg.Root.Meta[metaCapturedAt] != "" {
		node.Meta[metaCapturedAt] = sg.Root.Meta[metaCapturedAt]
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

// LearnedSkills returns the active skills this session should have on disk: its
// private skills and the project's shared ones. It is an optional capability,
// found by type assertion. On a name collision the private copy wins.
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

// SkillUsed notes that the named skills were used. It does nothing unless
// learned skills are on, and it is best-effort and silent.
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

// ApproveSkill sets or clears the approval mark on the skill at key. It works
// outside a session, like Restore. Revoking does not undo an earlier promotion;
// it stops the next one.
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
