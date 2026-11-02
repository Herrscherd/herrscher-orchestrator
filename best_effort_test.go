package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	contracts "github.com/Herrscherd/herrscher-contracts"
)

var errBoom = errors.New("boom")

type recordFailMem struct {
	*fakeMem
}

func (m recordFailMem) Record(context.Context, contracts.Node) error {
	return errBoom
}

type unlinkFailMem struct {
	*restoreMem
}

func (m unlinkFailMem) Unlink(context.Context, string, string) error {
	return errBoom
}

func TestRememberLogsAFailedRecordWithoutFailingTheTurn(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	c := New(recordFailMem{newFake()}, "alpha")
	c.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	c.remember(context.Background(), "use gofmt")

	out := logs.String()
	if !strings.Contains(out, "memory: remember failed") || !strings.Contains(out, "error=boom") {
		t.Fatalf("failed record not logged at warn, got %q", out)
	}
}

func TestRestoreReturnsAFailedUnlink(t *testing.T) {
	t.Parallel()
	mem := newRestoreMem()
	mem.nodes["fragment"] = contracts.Node{Key: "fragment", Meta: map[string]string{MetaMergedInto: "umbrella"}}

	_, err := Restore(context.Background(), unlinkFailMem{mem}, "fragment", Force(true))
	if !errors.Is(err, errBoom) {
		t.Fatalf("Restore error = %v, want the unlink failure", err)
	}
}

func TestIntSettingDefaultsWhenUnsetAndReportsInvalidValues(t *testing.T) {
	t.Parallel()
	unset := contracts.PluginConfig{}
	if got, err := intSetting(unset, "merge-max", 7); got != 7 || err != nil {
		t.Fatalf("unset setting = %d, %v; want the default 7", got, err)
	}

	bad := contracts.PluginConfig{Settings: map[string]string{"merge-max": "forty"}}
	_, err := intSetting(bad, "merge-max", 7)
	var settingErr SettingError
	if !errors.As(err, &settingErr) || settingErr.Key != "merge-max" || settingErr.Value != "forty" {
		t.Fatalf("invalid setting error = %v, want a SettingError naming merge-max", err)
	}
}

func TestApproveSkillRejectsANodeThatIsNotASkill(t *testing.T) {
	t.Parallel()
	m := newSkillMem()
	m.recalls["agents/a/skills/x"] = contracts.Subgraph{Root: contracts.Node{Key: "agents/a/skills/x", Kind: contracts.KindDecision}}

	err := ApproveSkill(context.Background(), m, "agents/a/skills/x", true)
	if !errors.Is(err, ErrNotSkill) {
		t.Fatalf("ApproveSkill error = %v, want ErrNotSkill", err)
	}
}
