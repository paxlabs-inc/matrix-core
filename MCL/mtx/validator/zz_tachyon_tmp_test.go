package validator

import (
	"os"
	"testing"

	"matrix/mcl/mtx/parser"
)

func TestTmpValidateTachyonEngineerSKILL(t *testing.T) {
	src, err := os.ReadFile("/root/matrix/skills/tachyon-engineer/SKILL.mtx")
	if err != nil {
		t.Fatalf("read skill: %v", err)
	}
	file, perrs := parser.New(src).Parse()
	if len(perrs) > 0 {
		t.Fatalf("parse error: %s", perrs[0])
	}
	if errs := ValidateSkill(file); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("validation error: %s", e)
		}
	}
}
