export default {
  currentYear: new Date().getFullYear(),
  title: "Matrix",
  tagline: "The Agent Operating Framework",
  description: "Matrix transforms natural-language requests into typed, inspectable Intent IR that an autonomous agent can execute deterministically. Built by PaxLabs on the Paxeer Network.",
  url: process.env.ELEVENTY_ENV === 'development' ? "http://localhost:8080" : "https://matrixmcl.com",
  author: { name: "PaxLabs Inc.", url: "https://labs.paxeer.app", x: "@paxlabs" },
  brand: {
    primaryColor: "#0565ff",
    backgroundColor: "#0A0A0A",
    accentColor: "#F59E0B",
    logoUrl: "/assets/images/matrix-logo.svg"
  },
  nav: [
    { label: "Features", url: "/features/" },
    { label: "Architecture", url: "/architecture/" },
    { label: "Use Cases", url: "/use-cases/" },
    { label: "Developers", url: "/developers/" },
    { label: "Enterprise", url: "/enterprise/" },
    { label: "Blog", url: "/blog/" }
  ],
  footerNav: {
    product: [
      { label: "Features", url: "/features/" },
      { label: "Architecture", url: "/architecture/" },
      { label: "Pricing", url: "/pricing/" },
      { label: "Use Cases", url: "/use-cases/" }
    ],
    developers: [
      { label: "Getting Started", url: "/developers/" },
      { label: "Documentation", url: "https://docs.matrixmcl.com", external: true },
      { label: "GitHub", url: "https://github.com/paxlabs-inc/matrix-core", external: true },
      { label: "API Reference", url: "https://docs.matrixmcl.com/api", external: true }
    ],
    company: [
      { label: "About", url: "/about/" },
      { label: "Blog", url: "/blog/" },
      { label: "Press", url: "/press/" },
      { label: "Contact", url: "/contact/" }
    ],
    legal: [
      { label: "Terms of Service", url: "/legal/terms/" },
      { label: "Privacy Policy", url: "/legal/privacy/" },
      { label: "License", url: "https://github.com/paxlabs-inc/matrix-core/blob/main/LICENSE.md", external: true }
    ]
  }
};
