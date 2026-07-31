package actorstate

import (
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/seatworker"
)

type SeatOutput = seatworker.SeatOutput
type InputCounts = seatworker.InputCounts

func Orient(packet contracts.WorkPacket) (SeatOutput, error) {
	return seatworker.Orient(packet)
}
