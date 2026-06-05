// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// driver.go — database/sql driver registration for the gateway.
//
// main.go deliberately imports nothing driver-specific (see its header)
// so the core import set stays auditable. But the credit_ledger is a
// HARD requirement for the metered launch posture: the in-memory ledger
// loses every actor's daily spend on restart, silently resetting budget
// caps. So we register the pure-Go lib/pq driver (driver name
// "postgres", matching the -postgres-driver default and the systemd
// unit) here.
//
// This is linked UNCONDITIONALLY rather than behind a `//go:build pq`
// tag on purpose: a build-tag stub reintroduces the "forgot the tag ->
// sql: unknown driver \"postgres\"" deploy footgun, which is exactly the
// kind of fleet-wide failure mode this launch must not ship. The cost is
// one small, well-audited, pure-Go dependency.
package main

import _ "github.com/lib/pq"

// Copyright © 2026 Paxlabs Inc. All rights reserved.
