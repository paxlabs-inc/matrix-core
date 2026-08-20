package audit

import (
	"centra/workforce/internal/auditorworker"
	"centra/workforce/internal/contracts"
)

type Decision = auditorworker.Decision

func Evaluate(packet contracts.VerdictPacket) (Decision, error) {
	return auditorworker.Evaluate(packet)
}
