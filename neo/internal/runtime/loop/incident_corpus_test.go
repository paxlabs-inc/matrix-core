// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import "testing"

func TestResurrectionIncidentLooptyLoop_SteerRejectCloseCycle(
	t *testing.T,
) {
	TestLiveLoopRetractsRejectedRepairBeforeCommittingValidAnswer(t)
}

func TestResurrectionIncidentO1BudgetKill_FinishedAnswerSurvivesBounds(
	t *testing.T,
) {
	TestFinishedAnswerSurvivesTheTokenBudget(t)
}

func TestResurrectionIncidentMoltbook_RecallPoisoningCannotPersist(
	t *testing.T,
) {
	TestCortexActivationRefreshAndSingleDeliveryChoke(t)
}

func TestResurrectionIncidentDroppedAnswer_FinalContentFlushesOnce(
	t *testing.T,
) {
	TestCortexActivationRefreshAndSingleDeliveryChoke(t)
}

func TestResurrectionIncidentNonconvergenceBurn_StopsWithHonestPartial(
	t *testing.T,
) {
	TestTokenBurnTerminatesAtTheBudgetWithAnHonestPartial(t)
}

func TestResurrectionIncidentIdentityLeak_ScrubbedBeforeDeliveryAndMemory(
	t *testing.T,
) {
	TestCortexActivationRefreshAndSingleDeliveryChoke(t)
}
