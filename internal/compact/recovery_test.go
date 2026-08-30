package compact

import (
	"strings"
	"testing"
	"time"
)

func TestRecoveryStateNilSafe(t *testing.T) {
	var s *RecoveryState
	s.RecordFileRead("/x", "ignored")
	s.RecordSkillInvocation("y", "ignored")
	if got := BuildRecoveryAttachment(s, nil); got != "" {
		t.Fatalf("expected empty attachment for nil state + no tools, got %q", got)
	}
}

func TestBuildRecoveryAttachmentEmits(t *testing.T) {
	s := NewRecoveryState()
	s.RecordFileRead("/tmp/a.go", "package a\n")
	s.RecordSkillInvocation("planner", "step 1\nstep 2\n")
	schemas := []map[string]any{
		{"name": "ReadFile", "description": "Read a file and return its contents.\nWith line numbers."},
		{"name": "Bash", "description": ""},
	}

	out := BuildRecoveryAttachment(s, schemas)
	if !strings.Contains(out, "/tmp/a.go") {
		t.Errorf("expected file path in attachment, got: %s", out)
	}
	if !strings.Contains(out, "planner") {
		t.Errorf("expected skill name in attachment, got: %s", out)
	}
	if !strings.Contains(out, "- ReadFile — Read a file and return its contents.") {
		t.Errorf("expected tool listing with first-line description, got: %s", out)
	}
	if !strings.Contains(out, "- Bash") {
		t.Errorf("expected bare tool listing without description, got: %s", out)
	}
	if !strings.Contains(out, "Note") {
		t.Errorf("expected closing note about not guessing from summary, got: %s", out)
	}
}

func TestRecoveryFileLimitAndOrder(t *testing.T) {
	s := NewRecoveryState()
	// 按时间分散记录 7 个文件，使「最新优先」的排序可被观察。
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 7; i++ {
		path := "/f" + string(rune('0'+i))
		s.RecordFileRead(path, "x")
		// 强制设置时间戳，保证排序是确定性的。
		rec := s.files[path]
		rec.Timestamp = base.Add(time.Duration(i) * time.Minute)
		s.files[path] = rec
	}
	out := BuildRecoveryAttachment(s, nil)
	// 只应出现最近 5 个。
	if strings.Count(out, "###") != 5 {
		t.Fatalf("expected 5 file sections, got: %d in %s", strings.Count(out, "###"), out)
	}
	// 最新在前：f6 必须排在 f2 之前。
	idxNew := strings.Index(out, "/f6")
	idxOld := strings.Index(out, "/f2")
	if idxNew < 0 || idxOld < 0 || idxNew > idxOld {
		t.Errorf("expected newest file (/f6) to appear before older (/f2); got idx new=%d old=%d", idxNew, idxOld)
	}
}

func TestRecoveryTruncatesPerFile(t *testing.T) {
	huge := strings.Repeat("x", int(float64(RecoveryTokensPerFile)*recoveryCharsPerToken)*3)
	s := NewRecoveryState()
	s.RecordFileRead("/big", huge)
	out := BuildRecoveryAttachment(s, nil)
	if !strings.Contains(out, "(content truncated)") {
		t.Errorf("expected truncation marker for oversize file, got prefix: %s", out[:200])
	}
}

func TestRecoverySkillsBudget(t *testing.T) {
	s := NewRecoveryState()
	// 6 个技能 × 5K-token 正文 ⇒ 总计 30K，必须止步于 25K 预算。
	bodyChars := int(float64(RecoveryTokensPerSkill) * recoveryCharsPerToken)
	body := strings.Repeat("y", bodyChars)
	base := time.Now()
	for i := 0; i < 6; i++ {
		name := "skill-" + string(rune('0'+i))
		s.RecordSkillInvocation(name, body)
		rec := s.skills[name]
		rec.Timestamp = base.Add(time.Duration(i) * time.Minute)
		s.skills[name] = rec
	}
	out := BuildRecoveryAttachment(s, nil)
	// 25K / 每个技能 5K = 最多 5 个。
	emitted := strings.Count(out, "### skill-")
	if emitted < 1 || emitted > 5 {
		t.Errorf("expected at most 5 skills under budget, emitted %d", emitted)
	}
}
