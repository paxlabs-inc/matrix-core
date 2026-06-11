import { FooterSimple } from "../../components/ui/smoothui/footer-1";

export function SiteFooter() {
  return (
    <div className="mt-24">
      <FooterSimple
        companyName="Deus"
        description="The open marketplace for data and agent services. Discover, call, and shippaid per use, settled natively on Paxeer."
        links={{
          product: [
            { name: "Discover", url: "/discover" },
            { name: "Catalog", url: "/catalog" },
          ],
          company: [
            { name: "Dashboard", url: "/dashboard" },
            { name: "List a service", url: "/dashboard/services/new" },
            { name: "Sign in", url: "/login" },
          ],
        }}
        social={{}}
        copyright={`© ${new Date().getFullYear()} Deus. All rights reserved. · Paxeer · chain 125`}
      />
      <EcosystemLinks />
      <LegalLinks />
    </div>
  );
}

/** Paxeer ecosystem destinations — external, open in a new tab. */
const ECOSYSTEM_LINKS = [
  { name: "Docs", url: "https://docs.paxeer.app/" },
  { name: "Developers", url: "https://www.paxeer.app/developers" },
  { name: "Whitepaper", url: "http://whitepaper.paxeer.app/" },
  { name: "GitHub", url: "https://github.com/Paxeer-Network" },
  { name: "Network", url: "https://www.paxeer.app/network" },
  { name: "Labs", url: "https://labs.paxeer.app/" },
  { name: "PaxScan", url: "https://paxscan.io/" },
  { name: "Status", url: "https://status.paxeer.app/" },
  { name: "Brand", url: "https://brand.paxeer.app" },
];

function EcosystemLinks() {
  return (
    <nav
      aria-label="Paxeer ecosystem"
      className="mx-auto flex max-w-7xl flex-wrap items-center justify-center gap-x-6 gap-y-2 px-6 pb-6 sm:px-8"
    >
      {ECOSYSTEM_LINKS.map((link) => (
        <a
          key={link.url}
          href={link.url}
          target="_blank"
          rel="noopener noreferrer"
          className="text-xs text-muted-foreground transition-colors hover:text-foreground"
        >
          {link.name}
        </a>
      ))}
    </nav>
  );
}

/**
 * Legal/compliance artifacts: static HTML in public/legal/, drop-in
 * replaceable without an app deploy.
 */
const LEGAL_LINKS = [
  { name: "Legal Hub", url: "/legal/index.html" },
  { name: "Marketplace Terms", url: "/legal/marketplace-terms-and-conditions.html" },
  { name: "Terms of Service", url: "/legal/terms-of-service.html" },
  { name: "Privacy Policy", url: "/legal/privacy-policy.html" },
  { name: "Acceptable Use", url: "/legal/acceptable-use-policy.html" },
  { name: "API Terms", url: "/legal/api-terms-of-use-license-agreement.html" },
  { name: "M2M Agreement", url: "/legal/machine-to-machine-m2m-agreement.html" },
  { name: "AI Responsible Use", url: "/legal/ai-agent-responsible-use-policy.html" },
  { name: "AML & KYC", url: "/legal/aml-kyc-policy.html" },
  { name: "Compliance Statement", url: "/legal/compliance-statement.html" },
  { name: "Governance & Liability", url: "/legal/consolidated-governance-and-liability-framework.html" },
  { name: "EU AI Act", url: "/legal/eu-ai-act-compliance.html" },
  { name: "On-Chain Data Privacy", url: "/legal/on-chain-data-privacy-notice.html" },
  { name: "Regulatory Overview", url: "/legal/regulatory-infrastructure-overview.html" },
  { name: "Compliance Training", url: "/legal/user-facing-compliance-training-centre.html" },
  { name: "Risk Disclosure", url: "/legal/risk-disclosure.html" },
  { name: "Refunds & Disputes", url: "/legal/refunds.html" },
  { name: "Security Policy", url: "/legal/security-policy.html" },
];

function LegalLinks() {
  return (
    <nav
      aria-label="Legal"
      className="mx-auto flex max-w-7xl flex-wrap items-center justify-center gap-x-6 gap-y-2 px-6 pb-10 sm:px-8"
    >
      {LEGAL_LINKS.map((link) => (
        <a
          key={link.url}
          href={link.url}
          className="text-xs text-muted-foreground transition-colors hover:text-foreground"
        >
          {link.name}
        </a>
      ))}
    </nav>
  );
}
