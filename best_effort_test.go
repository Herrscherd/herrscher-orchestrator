package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
	settingErr, ok := errors.AsType[*SettingError](err)
	if !ok || settingErr.Key != "merge-max" || settingErr.Value != "forty" {
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

type stubJournal struct {
	*bytes.Reader

	seekErr  error
	closeErr error
}

func (j stubJournal) Seek(offset int64, whence int) (int64, error) {
	if j.seekErr != nil {
		return 0, j.seekErr
	}
	return j.Reader.Seek(offset, whence)
}

func (j stubJournal) Close() error { return j.closeErr }

func (j stubJournal) Stat() (fs.FileInfo, error) { return nil, errBoom }

func journalLearner(journal string) (*Learner, *bytes.Buffer) {
	var logs bytes.Buffer
	l := NewLearner(newFake(), "s", contracts.MemoryScope{}, plainExt{}, journal, 0)
	l.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	return l, &logs
}

func assertJournalStepLogged(t *testing.T, logs *bytes.Buffer, step string) {
	t.Helper()
	if !strings.Contains(logs.String(), step) {
		t.Fatalf("want %q logged, got %q", step, logs.String())
	}
}

func TestReadJournalTailLogsAnOpenFailure(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l, logs := journalLearner(filepath.Join(blocker, "calls.log"))
	if got := l.readJournalTail(); got != "" {
		t.Fatalf("readJournalTail = %q, want empty on an open failure", got)
	}
	assertJournalStepLogged(t, logs, "memory: open journal failed")
}

func TestReadJournalTailStaysQuietWhenTheJournalIsAbsent(t *testing.T) {
	t.Parallel()
	l, logs := journalLearner(filepath.Join(t.TempDir(), "absent.log"))
	if got := l.readJournalTail(); got != "" {
		t.Fatalf("readJournalTail = %q, want empty for a missing journal", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("a missing journal is normal and must not be logged, got %q", logs.String())
	}
}

func TestReadJournalTailLogsAReadFailure(t *testing.T) {
	t.Parallel()
	l, logs := journalLearner(t.TempDir())
	if got := l.readJournalTail(); got != "" {
		t.Fatalf("readJournalTail = %q, want empty on a read failure", got)
	}
	assertJournalStepLogged(t, logs, "memory: read journal failed")
}

func TestReadJournalTailLogsASeekFailure(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "calls.log")
	if err := os.WriteFile(journal, []byte("123 tool=roblox__script_write"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, logs := journalLearner(journal)
	l.readJournalTail()
	content := []byte("123 tool=roblox__script_write")
	l.openJournal = func(string) (journalSource, error) {
		return stubJournal{Reader: bytes.NewReader(content), seekErr: errBoom}, nil
	}
	if got := l.readJournalTail(); got != "" {
		t.Fatalf("readJournalTail = %q, want empty on a seek failure", got)
	}
	assertJournalStepLogged(t, logs, "memory: seek journal failed")
}

func TestReadJournalTailLogsACloseFailure(t *testing.T) {
	l, logs := journalLearner("virtual")
	l.openJournal = func(string) (journalSource, error) {
		return stubJournal{Reader: bytes.NewReader(nil), closeErr: errBoom}, nil
	}
	l.readJournalTail()
	assertJournalStepLogged(t, logs, "memory: close journal failed")
}
