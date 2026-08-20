package actorstate

import (
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/seatworker"
)

type SeatOutput = seatworker.SeatOutput
type InputCounts = seatworker.InputCounts

func Orient(packet contracts.WorkPacket) (SeatOutput, error) {
	return seatworker.Orient(packet)
}
