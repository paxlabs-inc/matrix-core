<div align="center">

# Matrix‑Protocol License

[![License](https://img.shields.io/badge/License-Matrix--Protocol-004CED?style=for-the-badge&labelColor=000000)](#)
[![SPDX](https://img.shields.io/badge/SPDX-LicenseRef--Paxlabs--Matrix--Protocol-004CED?style=for-the-badge&labelColor=000000)](#)
[![Copyleft](https://img.shields.io/badge/Copyleft-Required-FF6B35?style=for-the-badge&labelColor=000000)](#)

**Copyright © 2026 Paxlabs Inc. All rights reserved.**
`SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol`

</div>

---

## Summary *(non-binding)*

> You may **read, use, deploy, and integrate** Matrix‑Protocol.
>
> If you **Modify and distribute or deploy** the Modified version, you must release your changes under this same license.
>
> **No Commercial License is required** until you cross a Commercial Trigger:
> - **Charged Fees** exceed **US $100,000** in any rolling 12‑month period (or any single calendar month), **or**
> - **Liquidity Under Control** exceeds **US $10,000,000**.
>
> *This summary is for convenience only.*

---

## Table of Contents

1. [Definitions](#1-definitions)
2. [Grant](#2-grant)
3. [Copyleft for Modifications & Extensions](#3-copyleft-for-modifications--extensions)
4. [Non‑Commercial Free Use](#4-noncommercial-free-use)
5. [Commercial Trigger](#5-commercial-trigger)
6. [Audit](#6-audit)
7. [Additional Intellectual Property Terms](#7-additional-intellectual-property-terms)
8. [Warranty & Liability](#8-warranty--liability)
9. [Termination](#9-termination)
10. [Governing Law; Venue; Injunctions](#10-governing-law-venue-injunctions)
11. [Notices; Assignment; Entire Agreement](#11-notices-assignment-entire-agreement)
12. [Versioning](#12-versioning)

---

## 1. Definitions

**1.1 "Licensed Work"** means the Matrix‑Protocol stack as released in this repository, including:

- **(a)** the core Matrix‑Protocol execution engine, instruction/handler interfaces, example instruction libraries published by Paxlabs, SDK stubs, schemas, configuration, tests, tooling, bytecode/ABIs, and deployment scripts;
- **(b)** all documentation and technical specifications published by Paxlabs; and
- **(c)** all updates, patches, and new versions of the foregoing that Paxlabs publishes under this license.

**1.2 "Charged Fees"** means all monetary or in‑kind value (fiat, crypto, tokens, credits, rebates, or other consideration) that You or Your Affiliates directly or indirectly receive or accrue in connection with operating, offering, or providing access to any product or service that is powered by, routed through, or materially enabled by the Licensed Work, including without limitation:

- **(a)** swap/trade/execution fees, positive‑slippage capture, spreads, mark‑ups, retained priority/tips;
- **(b)** maker/taker rebates, order‑flow payments, routing/referral/affiliate fees, MEV/builder/bundle payments and other extractable‑value shares;
- **(c)** subscription, seat, usage, API, or platform fees attributable to the Licensed Work;
- **(d)** performance/incentive/carried‑interest fees, revenue shares, or similar participation; and
- **(e)** token grants, rewards, airdrops, distributions, or rebates received by or for You or Your Affiliates in consideration of or tied to such operations.

Charged Fees are measured on a gross‑receipts basis at fair‑market value in USD when received or accrued, include amounts received by agents/designees or wallets You control, and must be reasonably allocated for bundles.

> **Anti‑avoidance.** Relabeling, splitting, routing through Affiliates/Related Parties, offsetting, or deferring does not exclude amounts; Affiliates/common control are aggregated; the Control or Benefit Principle applies.

**1.3 "Commercial License"** means a separate written agreement between Paxlabs and You (and/or Your Affiliates) that grants You the right to engage in Commercial Use of the Licensed Work subject to negotiated terms, conditions, and fees.

**1.4 "Control or Benefit Principle."** Triggers and obligations apply where you or your Affiliates control the relevant activity or benefit economically from it (directly or through agents/DAOs under your direction).

**1.5 "Rolling Year"** means any period of twelve (12) consecutive months measured on a rolling basis.

**1.6 "Liquidity Under Control (LUC)"** means the aggregate fair‑market USD value of real, non‑synthetic, non‑levered, withdrawable assets that Your (or Your Affiliates'/agents') products, services, or code can instruct or cause to be moved or committed via the Licensed Work — *e.g.*, wallet balances under automated control, committed liquidity, or programmatic authorization — at the time assessed.

**1.7 "Modify"** (and **"Modified Work"**) means to change, fork, translate, extend, or create a derivative work of the Licensed Work, including:

- **(a)** altering source or bytecode;
- **(b)** creating plug‑ins/modules/instruction programs that run in the same program/runtime or EVM address space (e.g., static/dynamic linking, delegatecall/proxy patterns); or
- **(c)** bundling the Licensed Work and additions as a single product.

**1.8 "You"** (and **"Your"**) means the individual or legal entity exercising rights under this license, and its Affiliates. **"Affiliates"** are entities controlling, controlled by, or under common control with a party, directly or indirectly.

---

## 2. Grant

**2.1 Source and Object Use.** Subject to Sections 3–11, Paxlabs grants You a worldwide, non‑exclusive license to use, copy, and distribute unmodified source/object forms of the Licensed Work.

**2.2 Pure Caller Use (Integration‑Only / Non‑Modifying).** Pure Caller Use means building or operating products or services that interact with the Licensed Work solely by forming calldata, submitting transactions, or reading state through published ABIs, APIs, or RPC endpoints, without distributing any Modified Work.

Pure Caller Use is permitted under this License and does not, by itself, trigger any payment obligations or Commercial License. However, if in connection with Pure Caller Use You or Your Affiliates:

- **(a)** charge or retain any fees, spreads, rebates, incentives, or other consideration; or
- **(b)** meet any Trigger in [§5.2](#5-commercial-trigger);

such use constitutes Commercial Use and requires obtaining a Commercial License from Paxlabs. *Even where Pure Caller Use is not met, see [§5.3](#5-commercial-trigger) for the current enforcement waiver applicable to Volume Activities.*

**2.3 Audit/Research Safe Harbor.** Security auditors and researchers may compile, test, and report on the Licensed Work in the course of good‑faith security research.

**2.4 Attribution.** For any distribution, public display, public performance, publication, reporting, disclosure, or other public communication of any portion of the Licensed Work or any analysis, results, or outputs derived from the Licensed Work, You must preserve all existing copyright, license, and attribution notices included in the Licensed Work and must include a reasonable attribution identifying the source as:

> **HyperPaxeer — © Paxlabs Inc 2026**

*(or any successor notice included in the Licensed Work).*

---

## 3. Copyleft for Modifications & Extensions

**3.1** If you Modify or distribute any portion of the Licensed Work, you must:

| | Requirement |
|---|---|
| **A** | Publish under this same license (`LicenseRef-Paxlabs-Matrix-Protocol`), at no charge, complete corresponding source of any portions of Your work that modify, extend, incorporate, or otherwise rely on the Licensed Work. |
| **B** | Preserve existing copyright, license, and third‑party notices. |
| **C** | Add prominent attribution: **"Powered by Matrix‑Protocol — © Paxlabs Inc 2026"** in repository README and UI where applicable. |
| **D** | Clearly mark changes and date of change. |
| **E** | Provide build and deployment instructions sufficient for reproducibility. |

**3.2** This copyleft covers all forms of Modification, combination, or use of the Licensed Work in or with other code, products, or systems.

**3.3** The obligations in §§3.1 A–B apply only to components that are derivative of the Licensed Work. Independent code that simply calls, interfaces with, or is distributed alongside the Licensed Work is not subject to this requirement.

---

## 4. Non‑Commercial Free Use

Non‑commercial use — including experimentation, prototyping, hackathons, research, and community pilots — is **free of charge**, subject to:

- [§3](#3-copyleft-for-modifications--extensions) for any Modifications; and
- Provided that such use does not constitute or involve any activity described in [§5](#5-commercial-trigger).

---

## 5. Commercial Trigger

**5.1 Commercial Use.** Any Commercial Use of the Licensed Work requires a Commercial License from Paxlabs, unless otherwise expressly stated. Commercial Use means any use of the Licensed Work that provides, enables, or is integrated into any product, service, system, workflow, or operation from which You or Your Affiliates derive, or reasonably expect to derive, monetary or in‑kind commercial value, directly or indirectly, including through Charged Fees or other consideration.

**5.2 Triggers.** Without limiting §5.1, You (and Your Affiliates) **must obtain a Commercial License** from Paxlabs if any of the following occur:

> **A · Aggregated Fees Trigger**
> Your aggregated Charged Fees attributable to usage of the Licensed Work exceed **USD 100,000 in any Rolling Year**.

> **B · LUC Trigger**
> Your LUC exceeds **USD 10,000,000 at any time**.

> **C · Operator / Liquidity Provider Direct‑Use**
> You (or Your Affiliate) operate instruction programs or services — e.g., deploying, offering, or running products or services powered by, routed through, or materially enabled by the Licensed Work — or act as a Liquidity Provider that directly exercises the Licensed Work (bypassing Paxeer Network/Paxlabs or other permitted/licensed interfaces) to capture fees or value and, in doing so, satisfy Triggers A or B above.

You must aggregate commonly‑controlled/Affiliated entities; **no disaggregation, white‑labeling, or similar structuring to avoid a Trigger is permitted.** The Control or Benefit Principle applies.

**5.3 Volume‑Activities Waiver.** Notwithstanding §§5.1–5.2, Paxlabs presently waives enforcement of the Commercial Triggers for parties whose activities consist primarily of routing order flow, aggregation, arbitrage, or market‑making through the Licensed Work (**"Volume Activities"**), including where such parties:

- **(i)** trade with their own or third‑party capital; and/or
- **(ii)** charge or retain fees, spreads, rebates, or other compensation.

This waiver **is not a license**, creates no reliance rights, and is **revocable by Paxlabs at any time in its sole discretion**, including with respect to existing users, by:

- **(a)** public notice in the project repository; or
- **(b)** direct notice.

Upon notice of revocation, you must within **ten (10) days** cease the Volume Activities or obtain a Commercial License; continued use thereafter constitutes unauthorized Commercial Use. This waiver does not excuse past breaches unrelated to this subsection.

**5.4 Contact Requirement.** Crossing any Trigger or any other Commercial Use requires you to contact Paxlabs within **15 days** at **`license@Paxeer.app`** to execute a Commercial License. Commercial License terms are confidential and may change.

---

## 6. Audit

Once per year, Paxlabs may request an independent revenue/LUC audit (under NDA) at Paxlabs's expense; if under‑reporting exceeds **5%**, You reimburse reasonable audit costs in addition to other remedies.

Paxlabs may also request an additional attestation **"for cause"** (objective indications of a Trigger). You must reasonably cooperate with any such attestation, including providing accurate records, logs, and other information reasonably necessary.

---

## 7. Additional Intellectual Property Terms

**7.1 Patents.** Paxlabs grants a limited, non‑exclusive, worldwide license under Paxlabs's patent claims that read on the Licensed Work **solely to the extent necessary** to exercise the rights expressly granted to You under this License.

Nothing in this License grants You any right to patent, claim, or seek protection for:

- **(a)** the Licensed Work (whether modified or unmodified);
- **(b)** any Modification of the Licensed Work; or
- **(c)** any work or system that incorporates, combines with, or depends on the Licensed Work.

This patent license **terminates** if you (or your Affiliates) stop using the Licensed Work or assert any patent claim against Paxlabs or compliant users of the Licensed Work. No implied patent license is granted beyond this clause.

**7.2 Trademarks & Branding.** No rights are granted to use any Paxlabs / Matrix‑Protocol / Paxeer Network names, logos, or trademarks, or any "Powered by HyperPaxeer" or similar designation, except solely to make truthful statements of compatibility or integration. Any use must comply with Paxlabs's brand guidelines and may require separate written permission.

**7.3 Reservation of Rights.** Except as expressly granted, no other rights (by implication, estoppel, or otherwise) are granted in copyrights, patents, trade secrets, trademarks, or other IP.

**7.4 No Endorsement.** You must not suggest Paxlabs endorses or certifies Your product absent a written agreement.

---

## 8. Warranty & Liability

> **THE LICENSED WORK IS PROVIDED BY PAXLABS "AS IS" AND "AS AVAILABLE,"** WITHOUT WARRANTIES OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON‑INFRINGEMENT, AND THAT OPERATION WILL BE UNINTERRUPTED OR ERROR‑FREE.
>
> TO THE MAXIMUM EXTENT PERMITTED BY LAW, PAXLABS, ITS AFFILIATES AND CONTRIBUTORS ARE **NOT LIABLE** FOR INDIRECT, INCIDENTAL, SPECIAL, CONSEQUENTIAL, EXEMPLARY, OR PUNITIVE DAMAGES, OR LOST PROFITS / REVENUE / GOODWILL, ARISING FROM OR RELATED TO THIS LICENSE OR THE LICENSED WORK, EVEN IF ADVISED OF THE POSSIBILITY.

Nothing in this Section limits Paxlabs's ability to seek injunctive relief without bond in addition to other remedies or to enforce Your obligations under this License.

---

## 9. Termination

Material breach — including any breach of §§2–9 — not cured within **15 days** of notice terminates this License. Prior compliant distributions survive.

> **Surviving Sections:** §2.4 · §3 · §8 · §10 · §11.3 survive termination.

---

## 10. Governing Law; Venue; Injunctions

This License is governed by the **laws of the State of New York**, excluding conflict rules. The parties submit to the exclusive jurisdiction and venue of the state and federal courts located in **New York County, New York (SDNY)**.

Each party consents to injunctive relief (including specific performance) for actual or threatened breach.

---

## 11. Notices; Assignment; Entire Agreement

**11.1 Notices.** Legal or any other notices to Paxlabs:

> **`legal@Paxeer.app`** — subject line: **"HyperPaxeer Notice"**

**11.2 Third‑Party Components.** Portions of the Licensed Work may incorporate, bundle, or reference third‑party components governed by their own licenses. You must comply with those third‑party terms; nothing in this License limits rights granted by those licenses. Preserve all third‑party copyright and license notices. A list of such components and licenses is provided in **`THIRD_PARTY_NOTICES`** (and/or in file headers) and may be updated from time to time.

**11.3 Assignment.** You may not assign this License (by operation of law or otherwise) without Paxlabs's prior written consent; any unauthorized assignment is void. **Paxlabs may assign freely.**

**11.4 Entire Agreement.** This License is the entire agreement for the Licensed Work and supersedes prior understandings. If any provision is unenforceable, it will be modified to the minimum extent necessary to be enforceable; the remainder stays in effect. No waiver is effective unless in writing.

---

## 12. Versioning

Paxlabs may publish new or updated versions of this License from time to time. Each release of the Licensed Work is governed by the license version identified in the repository for that release. Paxlabs may also re‑release the Licensed Work, or any portion of it, under different license terms in future releases.

---

<div align="center">

### End of License

**Contact** · [`license@Paxeer.app`](mailto:license@Paxeer.app) · [`legal@Paxeer.app`](mailto:legal@Paxeer.app)

*Copyright © 2026 Paxlabs Inc. All rights reserved.*

</div>