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
      <LegalLinks />
    </div>
  );
}

/**
 * Legal/compliance artifacts: static HTML in public/legal/, drop-in
 * replaceable without an app deploy.
 */
const LEGAL_LINKS = [
  { name: "Terms", url: "/legal/terms.html" },
  { name: "Privacy", url: "/legal/privacy.html" },
  { name: "Acceptable Use", url: "/legal/acceptable-use.html" },
  { name: "Developer Agreement", url: "/legal/developer-agreement.html" },
  { name: "DMCA", url: "/legal/dmca.html" },
  { name: "Refunds & Disputes", url: "/legal/refunds.html" },
  { name: "Risk Disclosure", url: "/legal/risk-disclosure.html" },
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
