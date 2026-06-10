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
    </div>
  );
}
