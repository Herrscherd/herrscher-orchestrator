package orchestrator

import (
	"context"
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

// React handles the memory markers in a reply. <remember> stores a fact. <recall>
// runs its search at once and queues the hits for the next Context. <skill> is
// recorded and stripped only when learned skills are on; otherwise it stays in
// the reply. It returns the reply with the other markers stripped. It is
// best-effort: a memory failure never fails the turn.
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
	hits, err := c.search(ctx, query)
	c.logFailure("memory: recall failed", err)
	if room := maxPending - len(c.pending); len(hits) > room {
		hits = hits[:room]
	}
	c.pending = append(c.pending, hits...)
}

func (c *Curator) search(ctx context.Context, query string) ([]contracts.Node, error) {
	if scope := c.scopeOf(); scope.Project != "" {
		return contracts.RecallRelevant(ctx, c.mem, scope, query, recallK)
	}
	return c.mem.Search(ctx, contracts.Query{Text: query, Ranked: true, Limit: recallK})
}

func (c *Curator) remember(ctx context.Context, fact string) {
	if fact == "" {
		return
	}
	title := oneline(fact, maxContentChars)
	node := contracts.Node{Kind: contracts.KindDecision, Title: title, Body: fact}
	if scope := c.scopeOf(); scope.Project != "" {
		node.Key = factKey(scope.Project, title)
		c.logFailure("memory: remember failed", contracts.RecordShared(ctx, c.mem, scope, node))
		return
	}
	node.Key = factKey(c.session, title)
	c.logFailure("memory: remember failed", c.mem.Record(ctx, node))
}

func (c *Curator) frame(digest string) string {
	var builder strings.Builder
	builder.WriteString("<memory>\n")
	builder.WriteString(memoryPreamble)
	if c.learnedSkills {
		builder.WriteString(skillPreamble)
	}
	if digest != "" {
		builder.WriteString("\n\n")
		builder.WriteString(digest)
	}
	if len(c.pending) > 0 {
		builder.WriteString("\n\n## results of your last <recall>\n")
		for _, n := range c.pending {
			writeNode(&builder, n)
		}
		c.pending = nil
	}
	builder.WriteString("\n</memory>")
	return builder.String()
}

const (
	fnvOffsetBasis64 uint64 = 14695981039346656037
	fnvPrime64       uint64 = 1099511628211
)

func fnv1a64(text string) uint64 {
	hash := fnvOffsetBasis64
	for index := range len(text) {
		hash ^= uint64(text[index])
		hash *= fnvPrime64
	}
	return hash
}

func factKey(prefix, title string) string {
	return prefix + "/notes/" + slug(title) + "-" + strconv.FormatUint(fnv1a64(title), 16)
}

const maxSlugLen = 60

func slug(text string) string {
	var builder strings.Builder
	pendingSep := false
	for _, character := range strings.TrimSpace(strings.ToLower(text)) {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			if pendingSep && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			pendingSep = false
			builder.WriteRune(character)
			if builder.Len() >= maxSlugLen {
				return builder.String()
			}
		default:
			pendingSep = true
		}
	}
	if builder.Len() == 0 {
		return "note"
	}
	return builder.String()
}
