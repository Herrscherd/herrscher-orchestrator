package orchestrator

import (
	"context"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

const recallK = 5

const maxPending = 12

const memoryPreamble = "This is your persistent memory (session · project · agent), recalled across sessions. " +
	"Search it any time by emitting <recall>your query</recall>. Its hits arrive next turn. " +
	"Store a durable fact with <remember>the fact</remember>."

var (
	recallMarker   = regexp.MustCompile(`(?is)<\s*recall\s*>\s*(.+?)\s*<\s*/\s*recall\s*>`)
	rememberMarker = regexp.MustCompile(`(?is)<\s*remember\s*>\s*(.+?)\s*<\s*/\s*remember\s*>`)
)

var _ contracts.TurnReactor = (*Curator)(nil)

// React handles the in-band memory markers the model emits: <remember> stores a
// durable fact immediately, <recall> searches memory and stashes the hits for the
// next Context call. Both markers are stripped from the returned reply so the
// human never sees them. It is best-effort: a memory failure never breaks the
// turn, and it is a no-op when there is no Memory.
func (c *Curator) React(ctx context.Context, reply string) string {
	if c.mem == nil {
		return reply
	}
	for _, m := range rememberMarker.FindAllStringSubmatch(reply, -1) {
		c.remember(ctx, strings.TrimSpace(m[1]))
	}
	for _, m := range recallMarker.FindAllStringSubmatch(reply, -1) {
		c.recall(ctx, strings.TrimSpace(m[1]))
	}
	reply = rememberMarker.ReplaceAllString(reply, "")
	reply = recallMarker.ReplaceAllString(reply, "")
	if c.learnedSkills {
		for _, m := range skillMarker.FindAllStringSubmatch(reply, -1) {
			c.recordSkill(ctx, m[1], m[2])
		}
		reply = skillMarker.ReplaceAllString(reply, "")
	}
	return strings.TrimSpace(reply)
}

func (c *Curator) recall(ctx context.Context, query string) {
	if query == "" || len(c.pending) >= maxPending {
		return
	}
	var hits []contracts.Node
	if scope := c.scopeOf(); scope.Project != "" {
		hits, _ = contracts.RecallRelevant(ctx, c.mem, scope, query, recallK)
	} else {
		hits, _ = c.mem.Search(ctx, contracts.Query{Text: query, Ranked: true, Limit: recallK})
	}
	if room := maxPending - len(c.pending); len(hits) > room {
		hits = hits[:room]
	}
	c.pending = append(c.pending, hits...)
}

func (c *Curator) remember(ctx context.Context, fact string) {
	if fact == "" {
		return
	}
	title := oneline(fact, maxContentChars)
	node := contracts.Node{Kind: contracts.KindDecision, Title: title, Body: fact}
	if scope := c.scopeOf(); scope.Project != "" {
		node.Key = factKey(scope.Project, title)
		_ = contracts.RecordShared(ctx, c.mem, scope, node)
		return
	}
	node.Key = factKey(c.session, title)
	_ = c.mem.Record(ctx, node)
}

func (c *Curator) frame(digest string) string {
	var b strings.Builder
	b.WriteString("<memory>\n")
	b.WriteString(memoryPreamble)
	if c.learnedSkills {
		b.WriteString(skillPreamble)
	}
	if digest != "" {
		b.WriteString("\n\n")
		b.WriteString(digest)
	}
	if len(c.pending) > 0 {
		b.WriteString("\n\n## results of your last <recall>\n")
		for _, n := range c.pending {
			writeNode(&b, n)
		}
		c.pending = nil
	}
	b.WriteString("\n</memory>")
	return b.String()
}

func factKey(prefix, title string) string {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(title))
	return prefix + "/notes/" + slug(title) + "-" + strconv.FormatUint(sum.Sum64(), 16)
}

func slug(s string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.TrimSpace(strings.ToLower(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if pendingSep && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingSep = false
			b.WriteRune(r)
			if b.Len() >= 60 {
				return b.String()
			}
		default:
			pendingSep = true
		}
	}
	if b.Len() == 0 {
		return "note"
	}
	return b.String()
}
