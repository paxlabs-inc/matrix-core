package audit

import (
	"matrix/workforce/internal/auditorworker"
	"matrix/workforce/internal/contracts"
)

type Decision = auditorworker.Decision

func Evaluate(packet contracts.VerdictPacket) (Decision, error) {
	return auditorworker.Evaluate(packet)
}
