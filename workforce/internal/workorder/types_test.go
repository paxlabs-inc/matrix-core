package workorder

import "testing"

func TestParseAcceptanceCriterionFailsClosedToSemanticReview(t *testing.T) {
	semantic, err := ParseAcceptanceCriterion(
		0, "The result satisfies the stated business objective",
	)
	if err != nil {
		t.Fatal(err)
	}
	if semantic.ID != "acceptance-01" ||
		semantic.Kind != AcceptanceSemantic ||
		semantic.Description != "The result satisfies the stated business objective" {
		t.Fatalf("semantic criterion = %+v", semantic)
	}

	evidence, err := ParseAcceptanceCriterion(
		1, "evidence_hash: provider observation is content-addressed",
	)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ID != "acceptance-02" ||
		evidence.Kind != AcceptanceEvidenceHash ||
		evidence.Description != "provider observation is content-addressed" {
		t.Fatalf("evidence criterion = %+v", evidence)
	}

	if _, err := ParseAcceptanceCriterion(0, "evidence_hash:"); err == nil {
		t.Fatal("empty evidence_hash criterion was accepted")
	}
}
